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
/// §hello_ack `color_space`): sRGB primaries and transfer, BT.709 matrix, and the range.
/// The library's CPU path writes FULL whatever the picture is (#1056), so the element
/// always overwrites them. `frame` starts with the SOF.
pub fn write_sof_colour(frame: &mut [u8], full_range: bool) -> Result<(), String> {
    let word = |i: usize| u32::from_le_bytes(frame[i..i + 4].try_into().unwrap());
    if frame.len() < 8 || word(0) >> 31 == 0 || (word(4) >> 24) & 3 != 0 {
        return Err("the frame does not start with a start-of-frame header".into());
    }
    // Bits 27..30 of the second word: color_primaries, transfer_function,
    // ycbcr_transform, ycbcr_range (bitstream.md BitstreamSequenceHeader).
    let w1 = (word(4) & !(0xF << 27)) | (u32::from(!full_range) << 30);
    frame[4..8].copy_from_slice(&w1.to_le_bytes());
    Ok(())
}

/// The per-frame byte budget for `kbps` at `fps_n/fps_d` (codec-pyrowave.md §Rate: the
/// target over fps). A variable or unknown rate budgets at 60 fps.
pub fn frame_budget(kbps: u32, fps_n: i32, fps_d: i32) -> usize {
    let (n, d) = if fps_n > 0 && fps_d > 0 {
        (fps_n as u64, fps_d as u64)
    } else {
        (60, 1)
    };
    (u64::from(kbps) * 1000 * d / (8 * n)).max(1) as usize
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
            (1234u32 | 1 << 26 | 1 << 30 | 1 << 31).to_le_bytes()
        );
        write_sof_colour(&mut f, true).unwrap();
        assert_eq!(f[4..8], (1234u32 | 1 << 26 | 1 << 31).to_le_bytes());
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
        assert_eq!(frame_budget(200_000, 60, 1), 416_666);
        assert_eq!(frame_budget(200_000, 30, 1), 833_333);
        assert_eq!(frame_budget(200_000, 60000, 1001), 417_083);
        assert_eq!(frame_budget(200_000, 0, 1), 416_666);
        assert_eq!(frame_budget(0, 60, 1), 1);
    }

    #[test]
    fn psnr_of_one_code_value_everywhere_is_48_db() {
        assert!((psnr(&[10; 64], &[11; 64]) - 48.13).abs() < 0.01);
        assert!(psnr(&[3; 4], &[3; 4]).is_infinite());
    }
}
