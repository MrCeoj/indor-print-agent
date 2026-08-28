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
              It also has to let ordinary users read the token - see below.

.PARAMETER Mode
  Service (default) or LogonTask.

.PARAMETER Source
  Folder holding thr1ve-print-agent.exe. Defaults to this script's folder.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File install.ps1
  powershell -ExecutionPolicy Bypass -File install.ps1 -Mode LogonTask
  powershell -ExecutionPolicy Bypass -File install.ps1 -Source C:\path\to\unzipped
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

# Well-known SIDs. Every account name in Windows is localised - a Spanish
# install calls these Administradores, SISTEMA and Usuarios - and a script that
# hardcodes the English ones works on the machine it was written on and nowhere
# else. SIDs are the same in every locale.
$SID_ADMINS = 'S-1-5-32-544'
$SID_SYSTEM = 'S-1-5-18'
$SID_USERS  = 'S-1-5-32-545'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

<#
  Native programs go through Invoke-Native; none is called directly.

  PowerShell gets native commands wrong in two opposite ways at once, and both
  broke earlier versions of this installer:

  · A non-zero exit code is NOT an error PowerShell notices. $ErrorActionPreference
    does nothing about it, so a failed step sails on silently. That is how a
    failed icacls produced a terminal where nothing worked while the installer
    reported success.

  · Any output on stderr IS an error PowerShell notices - it becomes a
    NativeCommandError, which $ErrorActionPreference = 'Stop' promotes to a
    terminating one, and 2>$null does not reliably suppress it. That is how
    `schtasks /End` on a machine with no such task - the normal case on a first
    install - killed the script before it copied a single file.

  So: stderr is merged into the output and never raised, the exit code comes
  back for the caller to judge, and -IgnoreFailure marks the steps where
  "there was nothing there" is the expected answer.

  Where a real cmdlet exists it is used instead, which is why only icacls and
  the agent itself are invoked this way. Get-Service and the ScheduledTasks
  module have none of this behaviour.
#>
function Invoke-Native {
  param(
    [Parameter(Mandatory)][string]$Exe,
    [string[]]$Arguments = @(),
    [switch]$IgnoreFailure,
    [string]$FailureMessage
  )

  # Cleared first: if $Exe cannot be found at all, PowerShell raises its own
  # error and never sets an exit code, leaving whatever the previous command
  # happened to return. A stale 0 there would read as success.
  $global:LASTEXITCODE = 0

  $previous = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    $output = & $Exe @Arguments 2>&1 | Out-String
    $code = $LASTEXITCODE
  }
  finally {
    $ErrorActionPreference = $previous
  }

  if (-not $IgnoreFailure -and $code -ne 0) {
    $detail = $output.Trim()
    if ($detail) { Write-Host $detail }
    if ($FailureMessage) { throw $FailureMessage }
    throw "$Exe failed with exit code $code."
  }

  return [pscustomobject]@{ ExitCode = $code; Output = $output }
}

# A well-known SID as the name this machine calls it, for the places that will
# not take a SID.
function Get-WellKnownName {
  param([Parameter(Mandatory)][string]$Sid)
  return ([System.Security.Principal.SecurityIdentifier]$Sid).Translate(
    [System.Security.Principal.NTAccount]).Value
}

function Resolve-SourceDir {
  param([string]$Given)

  if ($Given) { return $Given }
  if ($PSScriptRoot) { return $PSScriptRoot }
  if ($MyInvocation.ScriptName) { return (Split-Path -Parent $MyInvocation.ScriptName) }
  # Last resort: wherever the prompt is. Correct when someone cd'd into the
  # unzipped folder first, which is the usual way this gets run by hand.
  return (Get-Location).Path
}

function Assert-Admin {
  $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
  $principal = New-Object Security.Principal.WindowsPrincipal($identity)
  if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this from an elevated PowerShell (Run as administrator).'
  }
}

# ProgramFiles and ProgramData are all but guaranteed, but an empty one would
# fail the same opaque way $PSScriptRoot did, so they get the same treatment.
$ProgramFilesDir = if ($env:ProgramFiles) { $env:ProgramFiles } else { 'C:\Program Files' }
$ProgramDataDir = if ($env:ProgramData) { $env:ProgramData } else { 'C:\ProgramData' }

$InstallDir = Join-Path $ProgramFilesDir 'Thr1vePrint'
$DataDir = Join-Path $ProgramDataDir 'Thr1vePrint'

# ---------------------------------------------------------------------------

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

$existingService = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existingService -and $existingService.Status -ne 'Stopped') {
  Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
}

$existingTask = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
if ($existingTask) {
  Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
}

Start-Sleep -Seconds 2

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$targetExe = Join-Path $InstallDir $ExeName
Copy-Item $sourceExe $targetExe -Force
Write-Host "Installed $targetExe"

# The data directory holds the token, so it is locked down to the accounts that
# need it and no further.
#
# **Grant before stripping inheritance, and never the other way round.** An
# earlier version did both in one icacls call; on a Spanish install the account
# names did not resolve, the strip had already happened, and the folder was left
# readable by nobody at all - which surfaced as a service that would not start,
# because config.json had become unreadable to LocalSystem itself.
#
# LogonTask mode has to go further and let ordinary users read it: the agent
# then runs as the cashier, and an unreadable config is the same dead end. That
# is a real reduction in secrecy, and an acceptable one - the token authorizes
# printing on this machine, and the person logged into this machine is the
# cashier doing the printing.
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

$grants = @("*${SID_ADMINS}:(OI)(CI)F", "*${SID_SYSTEM}:(OI)(CI)F")
if ($Mode -eq 'LogonTask') {
  $grants += "*${SID_USERS}:(OI)(CI)R"
}

$granted = Invoke-Native -Exe 'icacls' -IgnoreFailure -Arguments (@($DataDir, '/grant:r') + $grants)

if ($granted.ExitCode -ne 0) {
  # Do NOT strip inheritance after a failed grant - that is what locks everyone
  # out. An inherited ACL is less private than intended but entirely functional,
  # and the agent still runs.
  Write-Warning "Could not set permissions on $DataDir (icacls exit $($granted.ExitCode))."
  Write-Warning 'Continuing with inherited permissions. The token file is readable by any local administrator.'
}
else {
  $stripped = Invoke-Native -Exe 'icacls' -Arguments @($DataDir, '/inheritance:r') -IgnoreFailure
  if ($stripped.ExitCode -ne 0) {
    Write-Warning "Could not remove inherited permissions from $DataDir (icacls exit $($stripped.ExitCode))."
  }
}

if ($Mode -eq 'Service') {
  # A previous logon task would fight this one for the port.
  if ($existingTask) {
    Write-Host 'Removing the previous logon task...'
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
  }

  # Remove a previous registration rather than trying to reconfigure it: the
  # binary path may have moved, and a stale ImagePath is a service that starts
  # and immediately dies.
  if ($existingService) {
    Write-Host 'Removing the previous service registration...'
    Invoke-Native -Exe $targetExe -Arguments @('uninstall') -IgnoreFailure | Out-Null
  }

  Write-Host 'Registering the service...'
  $installed = Invoke-Native -Exe $targetExe -Arguments @('install') `
    -FailureMessage 'Service registration failed.'
  if ($installed.Output.Trim()) { Write-Host $installed.Output.Trim() }
}
else {
  # A previous service would fight this task for the port.
  if ($existingService) {
    Write-Host 'Removing the previous service registration...'
    Invoke-Native -Exe $targetExe -Arguments @('uninstall') -IgnoreFailure | Out-Null
  }

  Write-Host 'Registering a logon task...'
  # The token still has to exist, and `install` is what mints it - but this mode
  # does not want a service, so mint it explicitly instead.
  Invoke-Native -Exe $targetExe -Arguments @('token') `
    -FailureMessage 'Could not create the terminal token.' | Out-Null

  if ($existingTask) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
  }

  # Registered for the Users group rather than a named account, so it runs for
  # whoever is at the counter. No elevation: the agent binds a high loopback
  # port and reads a file, and asking a standard cashier account for admin
  # rights at every logon is a prompt nobody will keep answering.
  $action = New-ScheduledTaskAction -Execute $targetExe -Argument 'run'
  $trigger = New-ScheduledTaskTrigger -AtLogOn
  $principal = New-ScheduledTaskPrincipal -GroupId (Get-WellKnownName $SID_USERS) -RunLevel Limited
  $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries -StartWhenAvailable

  Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Principal $principal -Settings $settings -Force | Out-Null

  Start-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
}

Start-Sleep -Seconds 2

# `status` is the proof the thing actually works: it reads the config the agent
# reads and lists the printers it can see. A failure here is reported, not
# thrown - the install is done either way, and the message is more useful than
# a stack trace.
Write-Host ''
$status = Invoke-Native -Exe $targetExe -Arguments @('status') -IgnoreFailure
if ($status.Output.Trim()) { Write-Host $status.Output.Trim() }
if ($status.ExitCode -ne 0) {
  Write-Warning 'The agent could not read its own configuration. See Troubleshooting in README.md.'
}
Write-Host ''

$tokenResult = Invoke-Native -Exe $targetExe -Arguments @('token') -IgnoreFailure
$token = $tokenResult.Output.Trim()

if ($tokenResult.ExitCode -ne 0 -or -not $token) {
  Write-Warning 'Could not read the terminal token.'
  Write-Warning "Run this once the problem above is fixed:  & '$targetExe' token"
}
else {
  Write-Host '--------------------------------------------------------------'
  Write-Host ' Paste this into the printer panel at /caja on this terminal:'
  Write-Host ''
  Write-Host "   $token"
  Write-Host ''
  Write-Host ' It authorizes printing on this machine only. It is not a login.'
  Write-Host '--------------------------------------------------------------'
}
