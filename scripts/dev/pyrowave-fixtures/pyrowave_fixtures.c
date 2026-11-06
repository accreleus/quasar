// Golden PyroWave fixture generator (#1053, spec #1048). Runs ONLY inside the platform dev image
// (scripts/dev/pyrowave-fixtures/gen.sh): it links the pinned libpyrowave-shared from /opt/gst.
//
// For each (picture, size, budget) it synthesizes a YCbCr 4:2:0 picture with integer maths only
// (no libm, no rand: the same bytes on every machine), encodes it with the library's CPU-input
// path at a fixed per-frame byte budget, writes the bitstream as a PWV1 file and writes the
// library's own decode of that bitstream as a planar I420 file. manifest.py then hashes and scores.
//
//   usage: pyrowave_fixtures OUTDIR
//   writes: OUTDIR/<picture>_<W>x<H>.src.yuv        the source picture (once per picture and size)
//           OUTDIR/<picture>_<W>x<H>_<rate>.pwv1    PWV1Header + (u32 size, frame) x 1
//           OUTDIR/<picture>_<W>x<H>_<rate>.ref.yuv the reference decode, I420 (Y, Cb, Cr planes)
//           OUTDIR/device.json, budgets.tsv         scratch for manifest.py: the encoding device, each budget
#include <vulkan/vulkan.h>
#include <pyrowave.h>

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define CHECK(call) do { pyrowave_result r_ = (call); if (r_ < 0) { \
	fprintf(stderr, "%s:%d: %s failed: %d\n", __FILE__, __LINE__, #call, (int)r_); exit(1); } } while (0)

static uint32_t hash2(uint32_t x, uint32_t y, uint32_t seed)
{
	uint32_t h = x * 0x9E3779B1u ^ y * 0x85EBCA77u ^ seed * 0xC2B2AE3Du;
	h ^= h >> 15; h *= 0x2C1B3C6Du; h ^= h >> 12; h *= 0x297A2D39u; h ^= h >> 15;
	return h;
}

typedef struct { int w, h; uint8_t *p[3]; } Pic; // p[0] is w*h, p[1] and p[2] are (w/2)*(h/2)

typedef void (*Sampler)(int x, int y, int w, int h, int out[3]);

// "detail": a gradient sky, a noise texture band (luma and chroma noise), saturated hard-edged
// shapes (chroma edges), a 1-px line grating, a checker, diagonal stripes. Coordinates are
// fractions of the size so every size gets every feature. BT.709 limited-range code values.
static void sample_detail(int x, int y, int w, int h, int o[3])
{
	o[0] = 48 + (x * 120) / w + (y * 60) / h;
	o[1] = 98 + (x * 60) / w;
	o[2] = 150 - (y * 60) / h;
	if (y >= (h * 2) / 3) { // texture band
		o[0] = 70 + (int)(hash2(x, y, 1) & 255) * 100 / 255;
		o[1] = 96 + (int)(hash2(x / 2, y / 2, 2) & 63);
		o[2] = 96 + (int)(hash2(x / 2, y / 2, 3) & 63);
	}
	if (x >= w / 2 && x < (w * 5) / 8 && y >= h / 8 && y < h / 2) { // 1-px line grating
		o[0] = (x % 4 == 0) ? 235 : 30; o[1] = o[2] = 128;
	}
	if (x < w / 6 && y >= h / 2 && y < (h * 2) / 3) { // 2-px checker
		o[0] = (((x / 2) + (y / 2)) & 1) ? 235 : 20; o[1] = o[2] = 128;
	}
	if (x >= w / 6 && x < w / 2 && y >= h / 2 && y < (h * 2) / 3 && (x * 3 + y * 2) % 11 == 0) { // diagonals
		o[0] = 235; o[1] = o[2] = 128;
	}
	if (x >= w / 8 && x < (w * 3) / 8 && y >= h / 6 && y < h / 2) { // red box, white 2-px border
		int edge = x < w / 8 + 2 || x >= (w * 3) / 8 - 2 || y < h / 6 + 2 || y >= h / 2 - 2;
		o[0] = edge ? 235 : 82; o[1] = edge ? 128 : 90; o[2] = edge ? 128 : 240;
	}
	{ // green disc
		int dx = x - (w * 3) / 4, dy = y - h / 3, r = h / 5;
		if (dx * dx + dy * dy < r * r) { o[0] = 145; o[1] = 54; o[2] = 34; }
	}
}

// "flat": a neutral field, one smooth luma ramp, a flat saturated patch, a black bar. Almost
// every block is zero, so it exercises skipped blocks and the ballot bits.
static void sample_flat(int x, int y, int w, int h, int o[3])
{
	o[0] = 126; o[1] = o[2] = 128;
	if (y < h / 6) o[0] = 40 + (x * 160) / w;
	if (x >= w / 4 && x < w / 2 && y >= h / 3 && y < (h * 2) / 3) { o[0] = 100; o[1] = 90; o[2] = 200; }
	if (y >= (h * 5) / 6) o[0] = 16;
}

static Pic render(Sampler s, int w, int h)
{
	Pic p = { w, h, { malloc((size_t)w * h), malloc((size_t)(w / 2) * (h / 2)), malloc((size_t)(w / 2) * (h / 2)) } };
	for (int y = 0; y < h; y++)
		for (int x = 0; x < w; x++) {
			int o[3]; s(x, y, w, h, o);
			p.p[0][y * w + x] = (uint8_t)o[0];
		}
	for (int cy = 0; cy < h / 2; cy++)
		for (int cx = 0; cx < w / 2; cx++) {
			int sum[3] = { 0, 0, 0 };
			for (int k = 0; k < 4; k++) { // 2x2 box average (rounded) of the full-resolution chroma
				int o[3]; s(2 * cx + (k & 1), 2 * cy + (k >> 1), w, h, o);
				sum[1] += o[1]; sum[2] += o[2];
			}
			p.p[1][cy * (w / 2) + cx] = (uint8_t)((sum[1] + 2) / 4);
			p.p[2][cy * (w / 2) + cx] = (uint8_t)((sum[2] + 2) / 4);
		}
	return p;
}

static void write_file(const char *dir, const char *name, const void *a, size_t an, const void *b, size_t bn, const void *c, size_t cn)
{
	char path[1024];
	snprintf(path, sizeof path, "%s/%s", dir, name);
	FILE *f = fopen(path, "wb");
	if (!f) { perror(path); exit(1); }
	if ((an && fwrite(a, 1, an, f) != an) || (bn && fwrite(b, 1, bn, f) != bn) || (cn && fwrite(c, 1, cn, f) != cn)) { perror(path); exit(1); }
	if (fclose(f)) { perror(path); exit(1); }
}

static void write_device_json(const char *dir, pyrowave_device dev)
{
	VkPhysicalDevice phys;
	pyrowave_device_get_vk_device_handles(dev, NULL, &phys, NULL);
	VkPhysicalDeviceDriverProperties drv = { VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_DRIVER_PROPERTIES };
	VkPhysicalDeviceProperties2 pr = { VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROPERTIES_2, &drv };
	vkGetPhysicalDeviceProperties2(phys, &pr);
	char buf[2048];
	int n = snprintf(buf, sizeof buf,
		"{\"device_name\": \"%s\", \"device_type\": %d, \"vendor_id\": %u, \"api_version\": \"%u.%u.%u\", "
		"\"driver_name\": \"%s\", \"driver_info\": \"%s\"}\n",
		pr.properties.deviceName, (int)pr.properties.deviceType, pr.properties.vendorID,
		VK_API_VERSION_MAJOR(pr.properties.apiVersion), VK_API_VERSION_MINOR(pr.properties.apiVersion),
		VK_API_VERSION_PATCH(pr.properties.apiVersion), drv.driverName, drv.driverInfo);
	write_file(dir, "device.json", buf, (size_t)n, NULL, 0, NULL, 0);
}


// The encoder leaves the alignment bytes at the end of a block (and the unused high bits of the last
// sign byte) as whatever its GPU buffer held, so two runs differ there and nowhere else. The
// decoder ignores them (bitstream.md, `payload_words`). Zero them so the file is reproducible.
// The block's used size follows bitstream.md "Decoding 8x8 blocks"; it must round up to
// payload_words exactly, which also checks this reading of the layout.
static void canonicalize(uint8_t *frame, size_t size, const char *what)
{
	size_t pos = 8; // after the SOF
	while (pos < size) {
		uint32_t w0;
		memcpy(&w0, frame + pos, 4);
		const size_t bytes = (size_t)((w0 >> 16) & 0xFFF) * 4;
		const unsigned n = (unsigned)__builtin_popcount(w0 & 0xFFFF);
		if (w0 >> 31 || bytes < 8 || pos + bytes > size) { fprintf(stderr, "%s: bad block at %zu\n", what, pos); exit(1); }
		const uint8_t *blk = frame + pos;
		size_t off = 8 + 3 * (size_t)n, nz = 0;
		for (unsigned i = 0; i < n; i++) {
			uint16_t cw = (uint16_t)(blk[8 + 2 * i] | blk[9 + 2 * i] << 8);
			const unsigned base = blk[8 + 2 * n + i] & 0xF;
			for (unsigned sub = 0; sub < 8; sub++) {
				const unsigned planes = ((cw >> (2 * sub)) & 3) + base;
				uint8_t any = 0;
				if (off + planes > bytes) { fprintf(stderr, "%s: payload overruns block at %zu\n", what, pos); exit(1); }
				for (unsigned pl = 0; pl < planes; pl++) any |= blk[off++];
				nz += (size_t)__builtin_popcount(any);
			}
		}
		const size_t used = off + (nz + 7) / 8;
		if ((used + 3) / 4 * 4 != bytes) { fprintf(stderr, "%s: block at %zu uses %zu bytes, payload_words says %zu\n", what, pos, used, bytes); exit(1); }
		memset(frame + pos + used, 0, bytes - used);
		if (nz % 8) frame[pos + used - 1] &= (uint8_t)((1u << (nz % 8)) - 1);
		pos += bytes;
	}
}

static void encode_decode(const char *dir, pyrowave_device dev, const Pic *pic, const char *pname, const char *rate, size_t budget, FILE *tsv)
{
	fprintf(tsv, "%s_%dx%d_%s\t%zu\n", pname, pic->w, pic->h, rate, budget);
	const int w = pic->w, h = pic->h, cw = w / 2, ch = h / 2;
	pyrowave_cpu_buffer in = {
		.data = { pic->p[0], pic->p[1], pic->p[2] },
		.row_stride_in_bytes = { (size_t)w, (size_t)cw, (size_t)cw },
		.plane_size_in_bytes = { (size_t)w * h, (size_t)cw * ch, (size_t)cw * ch },
		.width = w, .height = h, .format = PYROWAVE_CPU_BUFFER_FORMAT_YUV420P,
	};

	pyrowave_encoder enc;
	pyrowave_encoder_create_info eci = { dev, w, h, PYROWAVE_CHROMA_SUBSAMPLING_420 };
	CHECK(pyrowave_encoder_create(&eci, &enc));
	pyrowave_rate_control rc = { budget };
	CHECK(pyrowave_encoder_encode_cpu(enc, &in, &rc));

	// One packet holding the whole frame (no datagram boundary): the contract's packing is B07's
	// job; the fixture is the frame's bitstream. The slack matches upstream's bench.
	const size_t cap = budget + 65536;
	uint8_t *bits = malloc(cap);
	size_t np = 0;
	CHECK(pyrowave_encoder_compute_num_packets(enc, cap, &np));
	if (np != 1) { fprintf(stderr, "%s %s: %zu packets, want 1\n", pname, rate, np); exit(1); }
	pyrowave_packet pk;
	size_t out_packets = 0;
	CHECK(pyrowave_encoder_packetize(enc, &pk, cap, &out_packets, bits, cap));
	if (out_packets != 1 || pk.offset != 0 || pk.size < 16 || pk.size > cap) { fprintf(stderr, "%s %s: bad packetization\n", pname, rate); exit(1); }
	uint8_t *frame = bits + pk.offset;
	canonicalize(frame, pk.size, pname);

	// PWV1Header (bitstream.md "Raw disk format"): the frame's own SOF with the reserved
	// fields (sequence, extended, total_blocks, code) cleared, then rate 60/1, 8-bit, version 1.
	uint32_t sof0, sof1;
	memcpy(&sof0, frame, 4); memcpy(&sof1, frame + 4, 4);
	if (!(sof0 >> 31)) { fprintf(stderr, "%s %s: frame does not start with a SOF\n", pname, rate); exit(1); }
	uint8_t head[28];
	const uint32_t hw[7] = { 0x31565750u, sof0 & 0x0FFFFFFFu, sof1 & 0xFC000000u, 60, 1, 8u | (1u << 8), (uint32_t)pk.size };
	memcpy(head, hw, 28); // little-endian host (x86-64 image); the last u32 is the frame size

	char name[256];
	snprintf(name, sizeof name, "%s_%dx%d_%s.pwv1", pname, w, h, rate);
	write_file(dir, name, head, 28, frame, pk.size, NULL, 0);

	// Reference decode with the library's decoder.
	pyrowave_decoder dec;
	pyrowave_decoder_create_info dci = { dev, w, h, PYROWAVE_CHROMA_SUBSAMPLING_420, false };
	CHECK(pyrowave_decoder_create(&dci, &dec));
	CHECK(pyrowave_decoder_push_packet(dec, frame, pk.size));
	if (!pyrowave_decoder_decode_is_ready(dec, false)) { fprintf(stderr, "%s %s: decoder not ready after the whole frame\n", pname, rate); exit(1); }
	uint8_t *o0 = calloc((size_t)w * h, 1), *o1 = calloc((size_t)cw * ch, 1), *o2 = calloc((size_t)cw * ch, 1);
	pyrowave_cpu_buffer outb = in;
	outb.data[0] = o0; outb.data[1] = o1; outb.data[2] = o2;
	CHECK(pyrowave_decoder_decode_cpu_buffer_synchronous(dec, &outb));
	snprintf(name, sizeof name, "%s_%dx%d_%s.ref.yuv", pname, w, h, rate);
	write_file(dir, name, o0, (size_t)w * h, o1, (size_t)cw * ch, o2, (size_t)cw * ch);

	pyrowave_decoder_destroy(dec);
	pyrowave_encoder_destroy(enc);
	free(o0); free(o1); free(o2); free(bits);
}

int main(int argc, char **argv)
{
	if (argc != 2) { fprintf(stderr, "usage: %s OUTDIR\n", argv[0]); return 2; }
	const char *dir = argv[1];

	uint32_t maj, min, pat;
	pyrowave_get_api_version(&maj, &min, &pat);
	if (maj != PYROWAVE_API_VERSION_MAJOR) { fprintf(stderr, "library API %u.%u.%u does not match the header\n", maj, min, pat); return 1; }

	pyrowave_device dev;
	CHECK(pyrowave_create_default_device(&dev));
	write_device_json(dir, dev);

	// 320x192 is a multiple of 32; 200x114 is not (edge blocks, an odd chroma height of 57).
	// Budgets are bytes per frame: "hi" 0.8 B/px (6.4 bpp); "lo" 0.2 B/px for detail and 0.1 for flat
	// (flat needs well under 0.8 B/px, so its lo budget is the one that binds).
	static const struct { const char *name; Sampler s; int lo_div; } pics[] = { { "detail", sample_detail, 5 }, { "flat", sample_flat, 10 } };
	static const int sizes[][2] = { { 320, 192 }, { 200, 114 } };
	char tsvpath[1024];
	snprintf(tsvpath, sizeof tsvpath, "%s/budgets.tsv", dir);
	FILE *tsv = fopen(tsvpath, "w");
	if (!tsv) { perror(tsvpath); return 1; }
	for (int pi = 0; pi < 2; pi++)
		for (int si = 0; si < 2; si++) {
			const int w = sizes[si][0], h = sizes[si][1];
			Pic pic = render(pics[pi].s, w, h);
			char name[256];
			snprintf(name, sizeof name, "%s_%dx%d.src.yuv", pics[pi].name, w, h);
			write_file(dir, name, pic.p[0], (size_t)w * h, pic.p[1], (size_t)(w / 2) * (h / 2), pic.p[2], (size_t)(w / 2) * (h / 2));
			encode_decode(dir, dev, &pic, pics[pi].name, "hi", (size_t)w * h * 4 / 5, tsv);
			if (pi == 0) encode_decode(dir, dev, &pic, pics[pi].name, "xhi", (size_t)w * h * 3 / 2, tsv);
			encode_decode(dir, dev, &pic, pics[pi].name, "lo", (size_t)w * h / pics[pi].lo_div, tsv);
			free(pic.p[0]); free(pic.p[1]); free(pic.p[2]);
		}
	if (fclose(tsv)) { perror(tsvpath); return 1; }
	pyrowave_device_destroy(dev);
	return 0;
}
