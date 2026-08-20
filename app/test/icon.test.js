import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const icoPath = join(dirname(fileURLToPath(import.meta.url)), "..", "build", "icon.ico");

// Windows 11 Start (All apps vs Recommended) only agrees when every mip is an opaque square.
const WIN_ICO_SIZES = [16, 20, 24, 32, 40, 48, 64, 128, 256];

function icoEntries(buf) {
  const count = buf.readUInt16LE(4);
  const out = [];
  let off = 6;
  for (let i = 0; i < count; i++) {
    const w = buf[off] || 256;
    const h = buf[off + 1] || 256;
    const bpp = buf.readUInt16LE(off + 6);
    const size = buf.readUInt32LE(off + 8);
    const offset = buf.readUInt32LE(off + 12);
    out.push({ w, h, bpp, blob: buf.subarray(offset, offset + size) });
    off += 16;
  }
  return out;
}

function pngColorType(blob) {
  if (blob[0] !== 0x89 || blob.toString("ascii", 1, 4) !== "PNG") return null;
  // signature 8 + len 4 + "IHDR" 4 + width 4 + height 4 + bitDepth 1 + colorType 1
  return blob[8 + 4 + 4 + 4 + 4 + 1];
}

describe("Windows .ico", () => {
  it("embeds every Start-menu size as an opaque RGB square", () => {
    const entries = icoEntries(readFileSync(icoPath));
    const sizes = entries.map((e) => `${e.w}x${e.h}`).sort();
    expect(sizes).toEqual(WIN_ICO_SIZES.map((s) => `${s}x${s}`).sort());
    for (const e of entries) {
      const colorType = pngColorType(e.blob);
      expect(colorType, `${e.w}x${e.h} should be a PNG`).not.toBeNull();
      // 2 = RGB (no alpha). 6 = RGBA, which Win11 All apps treats as a shaped icon.
      expect(colorType, `${e.w}x${e.h} color type`).toBe(2);
    }
  });
});
