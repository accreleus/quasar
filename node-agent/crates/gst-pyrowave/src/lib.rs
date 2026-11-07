//! `pyrowaveenc`: PyroWave encode as a GStreamer element (#1056, spec #1048, contract
//! `protocol/codec-pyrowave.md`). Input is 8-bit 4:2:0 YCbCr in system memory; each output
//! buffer is one whole frame of bitstream v1 (the SOF, then the 32×32 blocks, every packet
//! self-delimiting) and every frame is a keyframe.

use gstreamer as gst;
use gstreamer::glib;
use gstreamer::prelude::*;

pub mod ffi;
mod imp;

glib::wrapper! {
    pub struct PyroWaveEnc(ObjectSubclass<imp::PyroWaveEnc>)
        @extends gstreamer_video::VideoEncoder, gst::Element, gst::Object;
}

fn plugin_init(plugin: &gst::Plugin) -> Result<(), glib::BoolError> {
    // Registered whatever the host has: a missing library or GPU fails at READY→PAUSED
    // with the reason, which is what the codec probe reports.
    gst::Element::register(
        Some(plugin),
        "pyrowaveenc",
        gst::Rank::NONE,
        PyroWaveEnc::static_type(),
    )
}

gst::plugin_define!(
    pyrowave,
    env!("CARGO_PKG_DESCRIPTION"),
    plugin_init,
    env!("CARGO_PKG_VERSION"),
    "MIT/X11",
    env!("CARGO_PKG_NAME"),
    env!("CARGO_PKG_NAME"),
    "https://github.com/accreleus/quasar"
);

/// Writes the SOF's usability fields for Quasar's configuration (codec-pyrowave.md
/// §hello_ack `color_space`, erratum quasar-protocol#29): sRGB primaries and transfer,
/// BT.709 matrix, the range, and centre chroma siting. At the pin the library writes FULL
/// range whatever the picture is and siting from its input, and the browser decoder refuses
/// a left-sited SOF, so the element always overwrites them. `frame` starts with the SOF.
pub fn write_sof_colour(frame: &mut [u8], full_range: bool) -> Result<(), String> {
    let word = |i: usize| u32::from_le_bytes(frame[i..i + 4].try_into().unwrap());
    if frame.len() < 8 || word(0) >> 31 == 0 || (word(4) >> 24) & 3 != 0 {
        return Err("the frame does not start with a start-of-frame header".into());
    }
    // Bits 27..31 of the second word: color_primaries, transfer_function, ycbcr_transform,
    // ycbcr_range, chroma_siting (bitstream.md BitstreamSequenceHeader). All zero but range.
    let w1 = (word(4) & !(0x1F << 27)) | (u32::from(!full_range) << 30);
    frame[4..8].copy_from_slice(&w1.to_le_bytes());
    Ok(())
}

/// The per-frame byte budget for `kbps` at `fps_n/fps_d` (codec-pyrowave.md §Rate: the
/// target over fps), never above `raw_bytes`, the uncompressed frame. A variable or
/// unknown rate budgets at 60 fps.
pub fn frame_budget(kbps: u32, fps_n: i32, fps_d: i32, raw_bytes: usize) -> usize {
    let (n, d) = if fps_n > 0 && fps_d > 0 {
        (fps_n as u128, fps_d as u128)
    } else {
        (60, 1)
    };
    let budget = u128::from(kbps) * 1000 * d / (8 * n);
    budget.clamp(1, raw_bytes.max(1) as u128) as usize
}

/// What `set_format` refuses beyond the pad template: an odd size (4:2:0), a matrix other
/// than BT.709, and a `chroma-site` field other than centre (`jpeg`/`none`), since the SOF
/// always says centre (codec-pyrowave.md, erratum quasar-protocol#29). Reads the caps
/// field, not `VideoInfo`: an unset site reads as MPEG-2 there at 720p and above.
pub fn check_input(caps: &gst::CapsRef) -> Result<gstreamer_video::VideoInfo, String> {
    let info = gstreamer_video::VideoInfo::from_caps(caps).map_err(|e| e.to_string())?;
    let (w, h) = (info.width(), info.height());
    if w % 2 != 0 || h % 2 != 0 {
        return Err(format!("4:2:0 needs an even size, got {w}x{h}"));
    }
    if info.colorimetry().matrix() != gstreamer_video::VideoColorMatrix::Bt709 {
        return Err("PyroWave input must be BT.709".into());
    }
    let site = caps
        .structure(0)
        .and_then(|s| s.get::<&str>("chroma-site").ok());
    if let Some(site) = site.filter(|s| !matches!(*s, "jpeg" | "none")) {
        return Err(format!(
            "PyroWave input must be centre-sited, got chroma-site {site}"
        ));
    }
    Ok(info)
}

/// PSNR in dB of two 8-bit planes; infinite when identical.
pub fn psnr(a: &[u8], b: &[u8]) -> f64 {
    assert_eq!(a.len(), b.len());
    let sse: u64 = a
        .iter()
        .zip(b)
        .map(|(&x, &y)| (i64::from(x) - i64::from(y)).pow(2) as u64)
        .sum();
    if sse == 0 {
        return f64::INFINITY;
    }
    10.0 * (255.0f64 * 255.0 * a.len() as f64 / sse as f64).log10()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sof(w1: u32) -> Vec<u8> {
        let mut f = (1u32 << 31 | 5).to_le_bytes().to_vec();
        f.extend(w1.to_le_bytes());
        f
    }

    #[test]
    fn the_sof_colour_fields_follow_the_configuration_and_nothing_else_moves() {
        // total_blocks 1234, chroma 4:4:4, every colour field set, siting LEFT.
        let all = 1234 | 1 << 26 | 0xF << 27 | 1 << 31;
        let mut f = sof(all);
        write_sof_colour(&mut f, false).unwrap();
        assert_eq!(
            f[4..8],
            (1234u32 | 1 << 26 | 1 << 30).to_le_bytes(),
            "LIMITED, CENTER"
        );
        write_sof_colour(&mut f, true).unwrap();
        assert_eq!(f[4..8], (1234u32 | 1 << 26).to_le_bytes(), "FULL, CENTER");
        assert_eq!(f[0..4], (1u32 << 31 | 5).to_le_bytes());
    }

    #[test]
    fn a_frame_without_a_leading_sof_is_refused() {
        assert!(write_sof_colour(&mut [0u8; 4], true).is_err());
        assert!(write_sof_colour(&mut 0u64.to_le_bytes(), true).is_err());
        assert!(
            write_sof_colour(&mut sof(1 << 24), true).is_err(),
            "code 1 is not START_OF_FRAME"
        );
    }

    #[test]
    fn the_budget_is_the_target_over_the_frame_rate() {
        const RAW_4K: usize = 3840 * 2160 * 3 / 2;
        assert_eq!(frame_budget(200_000, 60, 1, RAW_4K), 416_666);
        assert_eq!(frame_budget(200_000, 30, 1, RAW_4K), 833_333);
        assert_eq!(frame_budget(200_000, 60000, 1001, RAW_4K), 417_083);
        assert_eq!(frame_budget(200_000, 0, 1, RAW_4K), 416_666);
        assert_eq!(frame_budget(0, 60, 1, RAW_4K), 1);
    }

    #[test]
    fn the_budget_never_exceeds_the_raw_frame_and_never_overflows() {
        let raw = 1920 * 1080 * 3 / 2;
        // 1/100 fps at the property's maximum: 125 GB a frame before the clamp.
        assert_eq!(frame_budget(10_000_000, 1, 100, raw), raw);
        assert_eq!(frame_budget(10_000_000, 1, i32::MAX, raw), raw);
        assert_eq!(frame_budget(u32::MAX, 1, i32::MAX, usize::MAX), usize::MAX);
    }

    fn check(caps: &str) -> Result<gstreamer_video::VideoInfo, String> {
        gst::init().unwrap();
        check_input(&caps.parse::<gst::Caps>().unwrap())
    }

    #[test]
    fn odd_sizes_other_matrices_and_off_centre_siting_are_refused() {
        let base = "video/x-raw,format=I420,framerate=60/1,colorimetry=bt709";
        assert!(check(&format!("{base},width=320,height=192")).is_ok());
        // No chroma-site field: VideoInfo would call this MPEG-2 from 720p up.
        for (w, h) in [(1280, 720), (1920, 1080), (3840, 2160)] {
            assert!(
                check(&format!("{base},width={w},height={h}")).is_ok(),
                "{w}x{h}"
            );
        }
        for site in ["jpeg", "none"] {
            let caps = format!("{base},width=320,height=192,chroma-site={site}");
            assert!(check(&caps).is_ok(), "{site}");
        }
        assert!(check(&format!("{base},width=321,height=192")).is_err());
        assert!(check(&format!("{base},width=320,height=191")).is_err());
        for site in ["mpeg2", "cosited", "dv"] {
            let caps = format!("{base},width=1920,height=1080,chroma-site={site}");
            assert!(check(&caps).is_err(), "{site}");
        }
        assert!(check("video/x-raw,format=I420,width=320,height=192,colorimetry=bt601").is_err());
    }

    #[test]
    fn psnr_of_one_code_value_everywhere_is_48_db() {
        assert!((psnr(&[10; 64], &[11; 64]) - 48.13).abs() < 0.01);
        assert!(psnr(&[3; 4], &[3; 4]).is_infinite());
    }
}
