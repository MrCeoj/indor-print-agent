#!/usr/bin/env node
/**
 * Turn a captured print job back into a PNG.
 *
 * This is the verification loop that makes the whole raster path developable
 * without Windows and without paper: run the agent on a Mac, print from the
 * app, and the dev sink drops the exact bytes the printer would have received
 * into ~/.thr1ve-print/jobs. Point this at one of them and look at the result.
 *
 * It reads only what it needs to: every `GS v 0` raster band in the stream,
 * stacked in order. Text commands, cuts and drawer pulses are skipped, because
 * on the current ticket there are none to render - the whole receipt is one
 * bitmap, which is the point of the redesign.
 *
 *   node decode-job.mjs ~/.thr1ve-print/jobs/20260828-141233.000-Ticket.bin
 *   node decode-job.mjs --latest
 */

import { deflateSync } from 'node:zlib';
import { readFileSync, writeFileSync, readdirSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';

const JOBS_DIR = join(homedir(), '.thr1ve-print', 'jobs');

function latestJob() {
	const files = readdirSync(JOBS_DIR)
		.filter((f) => f.endsWith('.bin'))
		.sort();
	if (files.length === 0) throw new Error(`No jobs in ${JOBS_DIR}`);
	return join(JOBS_DIR, files[files.length - 1]);
}

/** Pull every GS v 0 band out of an ESC/POS stream. */
function readBands(buf) {
	const bands = [];
	let width = 0;

	for (let i = 0; i + 8 <= buf.length; ) {
		if (!(buf[i] === 0x1d && buf[i + 1] === 0x76 && buf[i + 2] === 0x30)) {
			i++;
			continue;
		}
		const bytesPerRow = buf[i + 4] | (buf[i + 5] << 8);
		const rows = buf[i + 6] | (buf[i + 7] << 8);
		const start = i + 8;
		const size = bytesPerRow * rows;
		if (size === 0 || start + size > buf.length) break;

		width = Math.max(width, bytesPerRow * 8);
		bands.push({ bytesPerRow, rows, data: buf.subarray(start, start + size) });
		i = start + size;
	}

	return { bands, width };
}

/** Bands to an 8-bit greyscale raster, one byte per dot. */
function toGrey(bands, width) {
	const height = bands.reduce((n, b) => n + b.rows, 0);
	const grey = Buffer.alloc(width * height, 0xff);
	let y = 0;

	for (const band of bands) {
		for (let row = 0; row < band.rows; row++, y++) {
			for (let x = 0; x < band.bytesPerRow * 8; x++) {
				const bit = (band.data[row * band.bytesPerRow + (x >> 3)] >> (7 - (x & 7))) & 1;
				if (bit) grey[y * width + x] = 0x00;
			}
		}
	}

	return { grey, height };
}

const CRC_TABLE = Array.from({ length: 256 }, (_, n) => {
	let c = n;
	for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
	return c >>> 0;
});

function crc32(buf) {
	let c = 0xffffffff;
	for (const byte of buf) c = CRC_TABLE[(c ^ byte) & 0xff] ^ (c >>> 8);
	return (c ^ 0xffffffff) >>> 0;
}

function chunk(type, data) {
	const length = Buffer.alloc(4);
	length.writeUInt32BE(data.length);
	const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
	const crc = Buffer.alloc(4);
	crc.writeUInt32BE(crc32(body));
	return Buffer.concat([length, body, crc]);
}

/** Greyscale PNG, colour type 0, no filtering. */
function png(grey, width, height) {
	const ihdr = Buffer.alloc(13);
	ihdr.writeUInt32BE(width, 0);
	ihdr.writeUInt32BE(height, 4);
	ihdr[8] = 8; // bit depth
	ihdr[9] = 0; // greyscale

	const raw = Buffer.alloc((width + 1) * height);
	for (let y = 0; y < height; y++) {
		raw[y * (width + 1)] = 0; // filter: none
		grey.copy(raw, y * (width + 1) + 1, y * width, (y + 1) * width);
	}

	return Buffer.concat([
		Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
		chunk('IHDR', ihdr),
		chunk('IDAT', deflateSync(raw, { level: 9 })),
		chunk('IEND', Buffer.alloc(0))
	]);
}

const arg = process.argv[2];
if (!arg) {
	console.error('usage: node decode-job.mjs <job.bin | --latest>');
	process.exit(1);
}

const path = arg === '--latest' ? latestJob() : arg;
const { bands, width } = readBands(readFileSync(path));

if (bands.length === 0) {
	console.error(`No raster bands in ${path}. Is this a drawer pulse rather than a ticket?`);
	process.exit(1);
}

const { grey, height } = toGrey(bands, width);
const out = path.replace(/\.bin$/, '') + '.png';
writeFileSync(out, png(grey, width, height));

console.log(`${bands.length} band(s), ${width}x${height} dots -> ${out}`);
