// WGSL for the PyroWave decoder: a port of upstream Themaister/pyrowave 7fe0e266
// shaders/{wavelet_dequant.comp, idwt.comp, yuv2rgb.frag} (MIT), by way of the B00 spike (#1049).
// Precision follows upstream's PYROWAVE_PRECISION=1: fp32 maths, fp16 storage for levels 0-1
// when the device has shader-f16.

// A hostile block can dequantise past f16's range: clamp so it stores no Inf (and no NaN later).
const toHi = (hi: string) =>
  hi === "f16" ? "fn to_hi(v: f32) -> f16 { return f16(clamp(v, -65504.0, 65504.0)); }" : "fn to_hi(v: f32) -> f32 { return v; }";

/**
 * Dequantise every block of a frame, one 128-lane workgroup per 32x32 block. Without `subgroups`
 * (the fallback path, #1055) the sign-bit scan runs in workgroup memory instead. A block whose
 * offset is BLOCK_LOST is not written, so its coefficients stay those of the last frame that had
 * it: block concealment (amendment 55 §Loss).
 */
export function dequantWGSL(f16: boolean, subgroups: boolean): string {
  const HI = f16 ? "f16" : "f32";
  return /* wgsl */ `
${f16 ? "enable f16;" : ""}
${subgroups ? "enable subgroups;" : ""}
struct Band { block_offset: u32, blocks_x: u32, block_count: u32, out_base: u32,
              width: u32, height: u32, lo: u32, pad: u32 };
struct Params { num_bands: u32, wg_stride: u32, total_blocks: u32, p1: u32, bands: array<Band, 64> };
@group(0) @binding(0) var<uniform> params: Params;
@group(0) @binding(1) var<storage, read> offsets: array<u32>;
@group(0) @binding(2) var<storage, read> payload: array<u32>;
@group(0) @binding(3) var<storage, read_write> coef_hi: array<${HI}>;
@group(0) @binding(4) var<storage, read_write> coef_lo: array<f32>;
@group(0) @binding(5) var<storage, read> block_band: array<u32>;
${toHi(HI)}

const BLOCK_ZERO = 0xffffffffu;
const BLOCK_LOST = 0xfffffffeu;

var<workgroup> sh_plane_off: array<u32, 16>;
var<workgroup> sh_sign_off: u32;
var<workgroup> sh_scan: array<u32, 128>;
${subgroups ? "var<workgroup> sh_sgid: atomic<u32>;" : ""}

fn rd8(b: u32) -> u32 { return (payload[b >> 2u] >> ((b & 3u) * 8u)) & 0xffu; }
fn rd16(h: u32) -> u32 { return (payload[h >> 1u] >> ((h & 1u) * 16u)) & 0xffffu; }

fn decode_quant(q: u32) -> f32 {
  let e = 4 - i32(q >> 3u);
  let m = i32(q & 7u);
  return (1.0 / (8.0 * 1024.0 * 1024.0)) * f32((8 + m) * (1 << u32(20 + e)));
}

fn plane_bytes(cw: u32, qb: u32) -> u32 {
  let lsbs = cw & 0x5555u;
  var msbs = cw & 0xaaaau;
  msbs |= msbs >> 1u;
  return countOneBits(lsbs) + countOneBits(msbs) + qb * 8u;
}

@compute @workgroup_size(128)
fn main(@builtin(workgroup_id) wg: vec3u,
        ${subgroups ? "@builtin(subgroup_invocation_id) sgi: u32, @builtin(subgroup_size) sgs: u32" : "@builtin(local_invocation_index) li: u32"}) {
  let bi = wg.y * params.wg_stride + wg.x;
  if (bi >= params.total_blocks) { return; }
  let off = offsets[bi];
  if (off == BLOCK_LOST) { return; }
  let d = params.bands[block_band[bi]];

${
    subgroups
      ? `  // WGSL has no gl_SubgroupID and no fixed lane order: build upstream's linear lane index
  // (subgroup id * size + lane) from a workgroup-atomic ticket per subgroup.
  var mine = 0u;
  if (subgroupElect()) { mine = atomicAdd(&sh_sgid, 1u); }
  let sg_id = subgroupBroadcastFirst(mine);
  let li = sg_id * sgs + sgi;`
      : ""
  }

  let rel = bi - d.block_offset;
  let blk = vec2u(rel % d.blocks_x, rel / d.blocks_x);
  let sub = li & 7u;
  let bx = (li >> 3u) & 3u;
  let by = (li >> 5u) & 3u;
  let lb = by * 4u + bx;
  let coord = blk * 32u + 8u * vec2u(bx, by) + vec2u(4u * (sub >> 2u), 2u * (sub & 3u));

  var ballot = 0u;
  var qcode = 0u;
  if (off != BLOCK_ZERO) {
    ballot = payload[off] & 0xffffu;
    qcode = payload[off + 1u] & 0xffu;
  }
  let nset = countOneBits(ballot);
  let base = off * 4u + 8u + 3u * nset;

  // Byte cost of each 8x8 block -> where its magnitude planes start.
  var cost = 0u;
  if (li < 16u && extractBits(ballot, li, 1u) != 0u) {
    let lco = countOneBits(extractBits(ballot, 0u, li));
    cost = plane_bytes(rd16(off * 2u + 4u + lco), rd8(off * 4u + 8u + nset * 2u + lco) & 0xfu);
  }
  // shortcut: a subgroupInclusiveAdd scan is possible when lanes 0-15 share a subgroup (size >= 16);
  // B12's bench decides whether it comes back, with real-GPU tests to cover it.
  if (li < 16u) { sh_scan[li] = cost; }
  workgroupBarrier();
  for (var s = 1u; s < 16u; s <<= 1u) {
    var up = 0u;
    if (li < 16u && li >= s) { up = sh_scan[li - s]; }
    workgroupBarrier();
    if (li < 16u) { sh_scan[li] += up; }
    workgroupBarrier();
  }
  let incl = sh_scan[min(li, 15u)];
  workgroupBarrier();
  if (li < 16u) {
    sh_plane_off[li] = base + incl - cost;
    if (li == 15u) { sh_sign_off = 8u * (base + incl); }
  }
  workgroupBarrier();

  // Each lane decodes one 4x2 subblock (8 magnitudes, MSB plane first).
  var v: array<f32, 8>;
  var sig = 0u;
  if (extractBits(ballot, lb, 1u) != 0u) {
    let lco = countOneBits(extractBits(ballot, 0u, lb));
    let cw = rd16(off * 2u + 4u + lco);
    let cw2 = rd8(off * 4u + 8u + nset * 2u + lco);
    let qb = cw2 & 0xfu;
    if (cw != 0u) {
      let bit_off = 2u * sub;
      let lsbs = cw & 0x5555u;
      var msbs = cw & 0xaaaau;
      msbs |= msbs >> 1u;
      var bo = countOneBits(extractBits(lsbs, 0u, bit_off)) + countOneBits(extractBits(msbs, 0u, bit_off))
             + qb * sub + sh_plane_off[lb];
      let planes = qb + extractBits(cw, bit_off, 2u);
      var mag: array<u32, 8>;
      for (var q = i32(planes) - 1; q >= 0; q--) {
        let p = rd8(bo);
        bo++;
        for (var b = 0u; b < 8u; b++) { mag[b] |= ((p >> b) & 1u) << u32(q); }
      }
      let scale = decode_quant(qcode) * (f32((cw2 >> 4u) & 0xfu) / 8.0 + 0.25);
      for (var b = 0u; b < 8u; b++) {
        if (mag[b] != 0u) { v[b] = (f32(mag[b]) + 0.5) * scale; sig++; }
      }
    }
  }

  // Exclusive scan of significant counts over the block, in lane order -> sign bit offset.
${
  subgroups
    ? `  let nsg = 128u / sgs;
  let si = subgroupInclusiveAdd(sig);
  if (sgi == sgs - 1u) { sh_scan[sg_id] = si; }
  workgroupBarrier();
  if (li == 0u) { for (var i = 1u; i < nsg; i++) { sh_scan[i] += sh_scan[i - 1u]; } }
  workgroupBarrier();
  var excl = si - sig;
  if (sg_id != 0u) { excl += sh_scan[sg_id - 1u]; }`
    : `  // Upstream's scan_subgroups_fallback with one lane per subgroup: Hillis-Steele over 128 lanes.
  sh_scan[li] = sig;
  workgroupBarrier();
  for (var s = 1u; s < 128u; s <<= 1u) {
    var up = 0u;
    if (li >= s) { up = sh_scan[li - s]; }
    workgroupBarrier();
    sh_scan[li] += up;
    workgroupBarrier();
  }
  let excl = sh_scan[li] - sig;`
}

  let so = sh_sign_off + excl;
  var sw = payload[so / 32u];
  let m = so & 31u;
  if (m != 0u) { sw = (sw >> m) | (payload[so / 32u + 1u] << (32u - m)); }
  var sc = 0u;
  for (var b = 0u; b < 8u; b++) {
    if (v[b] != 0.0) {
      if (((sw >> sc) & 1u) != 0u) { v[b] = -v[b]; }
      sc++;
    }
  }

  // Element b sits at (b / 2, b % 2) inside the 4x2 subblock.
  for (var b = 0u; b < 8u; b++) {
    let p = coord + vec2u(b >> 1u, b & 1u);
    if (p.x < d.width && p.y < d.height) {
      let idx = d.out_base + p.y * d.width + p.x;
      if (d.lo != 0u) { coef_lo[idx] = v[b]; } else { coef_hi[idx] = to_hi(v[b]); }
    }
  }
}`;
}

/**
 * Inverse CDF 9/7 DWT for one level: four bands (bw x bh) -> one 2bw x 2bh image, z = component.
 * Each workgroup lifts a 32x32 output tile from a 40x40 interleaved tile (4-sample apron) in
 * workgroup memory. Upstream's mirrored-repeat textureGather equals whole-sample symmetric
 * extension of the interleaved signal, which mirror() does with plain loads.
 */
export function idwtWGSL(f16: boolean): string {
  const HI = f16 ? "f16" : "f32";
  return /* wgsl */ `
${f16 ? "enable f16;" : ""}
// mode: bit 0 = input in coef_lo; bits 1..2 = output 0 coef_hi, 1 coef_lo, 2 final plane.
struct C { in_base: vec4u, bw: u32, bh: u32, out_base: u32, mode: u32 };
@group(0) @binding(0) var<uniform> cs: array<C, 3>;
@group(0) @binding(1) var<storage, read_write> coef_hi: array<${HI}>;
@group(0) @binding(2) var<storage, read_write> coef_lo: array<f32>;
@group(0) @binding(3) var<storage, read_write> planes: array<f32>;
${toHi(HI)}

const ALPHA = -1.586134342059924;
const BETA = -0.052980118572961;
const GAMMA = 0.882911075530934;
const DELTA = 0.443506852043971;
const K = 1.230174104914001;
const INV_K = 1.0 / 1.230174104914001;
const T = 40u;   // tile incl. apron
const S = 41u;   // shared row stride

// Every lifting step runs across the tile in workgroup memory: per-thread arrays spilled to
// scratch on AMD and ran 25x slower (B00).
var<workgroup> sh: array<f32, ${40 * 41}>;

// Indices further out than the apron only feed outputs past the edge, which are discarded.
fn mirror(i: i32, n: i32) -> i32 {
  var m = abs(i);
  if (m >= n) { m = 2 * n - 2 - m; }
  return clamp(m, 0, n - 1);
}

fn load_in(i: u32, lo: bool) -> f32 {
  if (lo) { return coef_lo[i]; }
  return f32(coef_hi[i]);
}

fn step_v(li: u32, i0: u32, i1: u32, c: f32) {
  let n = ((i1 - i0) / 2u + 1u) * T;
  for (var k = li; k < n; k += 64u) {
    let o = (i0 + 2u * (k / T)) * S + k % T;
    sh[o] -= c * (sh[o - S] + sh[o + S]);
  }
  workgroupBarrier();
}

fn step_h(li: u32, i0: u32, i1: u32, c: f32) {
  let nc = (i1 - i0) / 2u + 1u;
  for (var k = li; k < nc * 32u; k += 64u) {
    let o = (4u + (k & 31u)) * S + i0 + 2u * (k >> 5u);
    sh[o] -= c * (sh[o - 1u] + sh[o + 1u]);
  }
  workgroupBarrier();
}

@compute @workgroup_size(64)
fn main(@builtin(workgroup_id) wg: vec3u, @builtin(local_invocation_index) li: u32) {
  let p = cs[wg.z];
  let in_lo = (p.mode & 1u) != 0u;
  let out_kind = p.mode >> 1u;
  let nx = i32(2u * p.bw);
  let ny = i32(2u * p.bh);
  let ox = i32(wg.x) * 32 - 4;
  let oy = i32(wg.y) * 32 - 4;
  for (var k = li; k < T * T; k += 64u) {
    let r = k / T;
    let c = k % T;
    let gx = u32(mirror(ox + i32(c), nx));
    let gy = u32(mirror(oy + i32(r), ny));
    let band = (gx & 1u) | ((gy & 1u) << 1u); // 0 LL, 1 HL, 2 LH, 3 HH
    // Both passes' K / 1/K pre-scale folded in (low = K, high = 1/K per axis).
    let sc = select(INV_K, K, (r & 1u) == 0u) * select(INV_K, K, (c & 1u) == 0u);
    sh[r * S + c] = sc * load_in(p.in_base[band] + (gy >> 1u) * p.bw + (gx >> 1u), in_lo);
  }
  workgroupBarrier();

  // Vertical first (upstream's order), then horizontal.
  step_v(li, 2u, 38u, DELTA);
  step_v(li, 3u, 37u, GAMMA);
  step_v(li, 4u, 36u, BETA);
  step_v(li, 5u, 35u, ALPHA);
  step_h(li, 2u, 38u, DELTA);
  step_h(li, 3u, 37u, GAMMA);
  step_h(li, 4u, 36u, BETA);
  step_h(li, 5u, 35u, ALPHA);

  let ow = 2u * p.bw;
  let oh = 2u * p.bh;
  for (var k = li; k < 1024u; k += 64u) {
    let x = wg.x * 32u + (k & 31u);
    let y = wg.y * 32u + (k >> 5u);
    if (x < ow && y < oh) {
      let v = sh[((k >> 5u) + 4u) * S + (k & 31u) + 4u];
      let o = p.out_base + y * ow + x;
      if (out_kind == 2u) {
        planes[o] = clamp(v + 0.5, 0.0, 1.0);
      } else if (out_kind == 1u) {
        coef_lo[o] = v;
      } else {
        coef_hi[o] = to_hi(v);
      }
    }
  }
}`;
}

/** BT.709 YCbCr -> RGB (amendment 55 §hello_ack), centre-sited 4:2:0 with bilinear chroma. */
export const yuvToRgbWGSL = /* wgsl */ `
struct P { aw: u32, w: u32, h: u32, limited: u32, off_cb: u32, off_cr: u32, cstride: u32, pad: u32 };
@group(0) @binding(0) var<uniform> p: P;
@group(0) @binding(1) var<storage, read> planes: array<f32>;

@vertex fn vs(@builtin(vertex_index) i: u32) -> @builtin(position) vec4f {
  let xy = vec2f(f32((i << 1u) & 2u), f32(i & 2u));
  return vec4f(xy * 2.0 - 1.0, 0.0, 1.0);
}

// x, y: the fragment position, at pixel centres. Upstream samples the half-size plane at the
// same UV, so chroma texel i sits between luma pixels 2i and 2i + 1.
fn chroma(off: u32, x: f32, y: f32) -> f32 {
  let cw = i32((p.w + 1u) / 2u); // the picture's chroma, not the plane's padded stride
  let ch = i32((p.h + 1u) / 2u);
  let fx = x * 0.5 - 0.5;
  let fy = y * 0.5 - 0.5;
  let x0 = i32(floor(fx));
  let y0 = i32(floor(fy));
  let tx = fx - f32(x0);
  let ty = fy - f32(y0);
  let xa = u32(clamp(x0, 0, cw - 1)); let xb = u32(clamp(x0 + 1, 0, cw - 1));
  let ya = u32(clamp(y0, 0, ch - 1)); let yb = u32(clamp(y0 + 1, 0, ch - 1));
  let s = p.cstride;
  let a = mix(planes[off + ya * s + xa], planes[off + ya * s + xb], tx);
  let b = mix(planes[off + yb * s + xa], planes[off + yb * s + xb], tx);
  return mix(a, b, ty);
}

@fragment fn fs(@builtin(position) pos: vec4f) -> @location(0) vec4f {
  var Y = planes[u32(pos.y) * p.aw + u32(pos.x)];
  var cb = chroma(p.off_cb, pos.x, pos.y) - 128.0 / 255.0;
  var cr = chroma(p.off_cr, pos.x, pos.y) - 128.0 / 255.0;
  if (p.limited != 0u) {
    Y = clamp((Y - 16.0 / 255.0) * (255.0 / 219.0), 0.0, 1.0);
    cb = clamp(cb * (255.0 / 224.0), -0.5, 0.5);
    cr = clamp(cr * (255.0 / 224.0), -0.5, 0.5);
  }
  let m = mat3x3f(vec3f(1.0, 1.0, 1.0), vec3f(0.0, -0.13397432 / 0.7152, 1.8556), vec3f(1.5748, -0.33480248 / 0.7152, 0.0));
  return vec4f(clamp(m * vec3f(Y, cb, cr), vec3f(0.0), vec3f(1.0)), 1.0);
}`;
