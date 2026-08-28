<#
.SYNOPSIS
  Install the Thr1ve Print Agent on a front desk terminal.

.DESCRIPTION
  Copies the binary into Program Files, registers it, starts it, and prints the
  token the cashier pastes into the printer panel at /caja.

  Two modes, and the difference matters:

    Service   (default) Runs under LocalSystem, starts at boot, restarts on
              failure, and keeps running when nobody is logged in. It runs in
              session 0, which cannot see printers installed for a single user
              account - only machine-wide ones. USB receipt printers normally
              install machine-wide, so this is almost always the right choice.

    LogonTask Runs in the cashier's own session at logon, so it sees exactly
              the printers the cashier sees. Use this when `agent status`
              reports no printers on a terminal where printing plainly works.

.PARAMETER Mode
  Service (default) or LogonTask.

.PARAMETER Source
  Folder holding thr1ve-print-agent.exe. Defaults to this script's folder.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File install.ps1
  powershell -ExecutionPolicy Bypass -File install.ps1 -Mode LogonTask
#>

[CmdletBinding()]
param(
  [ValidateSet('Service', 'LogonTask')]
  [string]$Mode = 'Service',

  # Resolved below rather than defaulted to $PSScriptRoot here. That variable is
  # empty whenever the script is not being run as a file - pasted into the
  # console, dot-sourced, piped through iex - and an empty default turns the
  # first Join-Path into "Cannot bind argument to parameter 'Path' because it is
  # an empty string", which says nothing about what actually went wrong.
  [string]$Source = ''
)

$ErrorActionPreference = 'Stop'

$ServiceName = 'Thr1vePrint'
$ExeName     = 'thr1ve-print-agent.exe'
$TaskName    = 'Thr1vePrintAgent'

function Resolve-SourceDir {
  param([string]$Given)

  if ($Given) { return $Given }
  if ($PSScriptRoot) { return $PSScriptRoot }
  if ($MyInvocation.ScriptName) { return (Split-Path -Parent $MyInvocation.ScriptName) }
  # Last resort: wherever the prompt is. Correct when someone cd'd into the
  # unzipped folder first, which is the usual way this gets run by hand.
  return (Get-Location).Path
}

# ProgramFiles and ProgramData are all but guaranteed, but an empty one here
# would fail the same opaque way, so they get the same treatment.
$ProgramFilesDir = if ($env:ProgramFiles) { $env:ProgramFiles } else { 'C:\Program Files' }
$ProgramDataDir  = if ($env:ProgramData)  { $env:ProgramData }  else { 'C:\ProgramData' }

$InstallDir = Join-Path $ProgramFilesDir 'Thr1vePrint'
$DataDir    = Join-Path $ProgramDataDir  'Thr1vePrint'

function Assert-Admin {
  $identity  = [Security.Principal.WindowsIdentity]::GetCurrent()
  $principal = New-Object Security.Principal.WindowsPrincipal($identity)
  if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this from an elevated PowerShell (Run as administrator).'
  }
}

Assert-Admin

$Source = Resolve-SourceDir -Given $Source
Write-Host "Source folder: $Source"

$sourceExe = Join-Path $Source $ExeName
if (-not (Test-Path $sourceExe)) {
  throw @"
$ExeName not found in:
  $Source

Unzip the release and run this from that folder, or point at it explicitly:
  .\install.ps1 -Source C:\path\to\unzipped
"@
}

# Stop whatever is already running, so the copy below is not blocked by a file
# in use. Both are best-effort: a fresh terminal has neither.
Write-Host 'Stopping any previous agent...'
& sc.exe stop $ServiceName   2>$null | Out-Null
& schtasks /End /TN $TaskName 2>$null | Out-Null
Start-Sleep -Seconds 2

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$targetExe = Join-Path $InstallDir $ExeName
Copy-Item $sourceExe $targetExe -Force
Write-Host "Installed $targetExe"

# The data directory holds the token. Administrators and SYSTEM only: a cashier
# account has no reason to read it, and the ACL is the only thing that stops a
# curious user copying a credential off the counter.
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
& icacls $DataDir /inheritance:r /grant:r 'Administrators:(OI)(CI)F' 'SYSTEM:(OI)(CI)F' | Out-Null

if ($Mode -eq 'Service') {
  # Remove a previous registration rather than trying to reconfigure it: the
  # binary path may have moved, and a stale ImagePath is a service that starts
  # and immediately dies.
  & sc.exe query $ServiceName *> $null
  if ($LASTEXITCODE -eq 0) {
    Write-Host 'Removing the previous service registration...'
    & $targetExe uninstall
  }

  Write-Host 'Registering the service...'
  & $targetExe install
  if ($LASTEXITCODE -ne 0) { throw 'Service registration failed.' }
}
else {
  Write-Host 'Registering a logon task...'
  # The token still has to exist, and `install` is what mints it - but this
  # mode does not want a service, so mint it explicitly instead.
  & $targetExe token | Out-Null

  & schtasks /Delete /TN $TaskName /F 2>$null | Out-Null
  & schtasks /Create /TN $TaskName /TR "`"$targetExe`" run" /SC ONLOGON /RL HIGHEST /F
  if ($LASTEXITCODE -ne 0) { throw 'Creating the logon task failed.' }
  & schtasks /Run /TN $TaskName | Out-Null
}

Start-Sleep -Seconds 2

Write-Host ''
& $targetExe status
Write-Host ''

$token = (& $targetExe token).Trim()
Write-Host '--------------------------------------------------------------'
Write-Host ' Paste this into the printer panel at /caja on this terminal:'
Write-Host ''
Write-Host "   $token"
Write-Host ''
Write-Host ' It authorizes printing on this machine only. It is not a login.'
Write-Host '--------------------------------------------------------------'
