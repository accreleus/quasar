// Writes .tar.gz and .zip archives with nothing but Node (make e2e-pack: the client half must run where only Node is installed).
// A member is { name, data: Buffer, mode?: 0o644 }. Names are checked on the way in: relative, "/" separated, no "..", no drive or
// leading slash, no NUL or backslash; nothing here follows or writes a symlink.
import zlib from "node:zlib";

export function checkName(name) {
  if (typeof name !== "string" || name === "" || name.length > 100 || /[\0\\]/.test(name) || name.startsWith("/") || /^[A-Za-z]:/.test(name)
    || name.split("/").some((p) => p === "" || p === "." || p === "..")) {
    throw new Error(`unsafe archive member name ${JSON.stringify(String(name).slice(0, 60))}`);
  }
  return name;
}

const octal = (n, width) => n.toString(8).padStart(width - 1, "0") + "\0";

/** A ustar archive, gzipped. `mtime` (seconds) is fixed by default so the same files make the same bytes. */
export function tarGz(members, { mtime = 0 } = {}) {
  const parts = [];
  for (const m of members) {
    checkName(m.name);
    const h = Buffer.alloc(512);
    h.write(m.name, 0, "utf8");
    h.write(octal(m.mode ?? 0o644, 8), 100);
    h.write(octal(0, 8), 108);
    h.write(octal(0, 8), 116);
    h.write(octal(m.data.length, 12), 124);
    h.write(octal(mtime, 12), 136);
    h.write("        ", 148); // the checksum is summed with its own field as spaces
    h.write("0", 156);
    h.write("ustar\0" + "00", 257);
    h.write(octal([...h].reduce((a, b) => a + b, 0), 7).slice(0, 6) + "\0 ", 148);
    parts.push(h, m.data, Buffer.alloc((512 - (m.data.length % 512)) % 512));
  }
  parts.push(Buffer.alloc(1024));
  return zlib.gzipSync(Buffer.concat(parts), { level: 9 });
}

const CRC = Uint32Array.from({ length: 256 }, (_, n) => { let c = n; for (let k = 0; k < 8; k += 1) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; return c >>> 0; });
// zlib.crc32 only exists from Node 22.2; the remote machine may have 22.0.
export const crc32 = (buf) => { let c = 0xffffffff; for (const b of buf) c = CRC[(c ^ b) & 0xff] ^ (c >>> 8); return (c ^ 0xffffffff) >>> 0; };

/** A zip archive (deflate, no zip64: one file or the whole archive over 4 GB is refused). `date` stamps every member. */
export function zip(members, { date = new Date(Date.UTC(1980, 0, 1)) } = {}) {
  const dosTime = (date.getUTCHours() << 11) | (date.getUTCMinutes() << 5) | (date.getUTCSeconds() >> 1);
  const dosDate = ((date.getUTCFullYear() - 1980) << 9) | ((date.getUTCMonth() + 1) << 5) | date.getUTCDate();
  const local = [];
  const central = [];
  let offset = 0;
  for (const m of members) {
    const name = Buffer.from(checkName(m.name), "utf8");
    const body = zlib.deflateRawSync(m.data, { level: 9 });
    const h = Buffer.alloc(30);
    h.writeUInt32LE(0x04034b50, 0);
    h.writeUInt16LE(20, 4); // version needed
    h.writeUInt16LE(0x0800, 6); // UTF-8 names
    h.writeUInt16LE(8, 8); // deflate
    h.writeUInt16LE(dosTime, 10);
    h.writeUInt16LE(dosDate, 12);
    h.writeUInt32LE(crc32(m.data), 14);
    h.writeUInt32LE(body.length, 18);
    h.writeUInt32LE(m.data.length, 22);
    h.writeUInt16LE(name.length, 26);
    const c = Buffer.alloc(46);
    c.writeUInt32LE(0x02014b50, 0);
    c.writeUInt16LE((3 << 8) | 20, 4); // made by Unix, so the mode below is read back
    h.copy(c, 6, 4, 30); // version needed .. name length are the same fields
    c.writeUInt32LE(((0o100000 | (m.mode ?? 0o644)) << 16) >>> 0, 38);
    c.writeUInt32LE(offset, 42);
    local.push(h, name, body);
    central.push(c, name);
    offset += h.length + name.length + body.length;
  }
  const cd = Buffer.concat(central);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(members.length, 8);
  end.writeUInt16LE(members.length, 10);
  end.writeUInt32LE(cd.length, 12);
  end.writeUInt32LE(offset, 16);
  if (offset + cd.length > 0xffffffff || members.length > 0xffff) throw new Error("archive too large for a zip without zip64");
  return Buffer.concat([...local, cd, end]);
}
