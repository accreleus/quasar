#!/bin/bash
# Sweep: per resolution and bitrate (Mbit/s at 60 fps) -> bench timings, size, PSNR.
cd /root/pyrowave/build
mkdir -p /root/out
echo "res,mbps,bytes_per_frame,enc_us,dec_us,dequant_us,idwt_us,dwt_us,quant_us,analyze_us,resolve_us,packing_us,actual_bytes_avg,psnr"
for res in 1080p 1440p 2160p; do
  for mbps in 100 200 300 400; do
    bytes=$(( mbps * 1000000 / 8 / 60 ))
    log=$(PW_BYTES=$bytes ./pyrowave-bench /root/frames/game_$res.y4m 2>&1)
    g() { echo "$log" | grep -E "^\[INFO\]: $1 ->" | head -1 | awk '{print $(NF-2)}'; }
    ./pyrowave-encode /root/frames/game_$res.y4m /root/out/game_${res}_${mbps}.pwv1 $bytes >/dev/null 2>&1
    ./pyrowave-decode /root/out/game_${res}_${mbps}.pwv1 /root/out/game_${res}_${mbps}_dec.y4m >/dev/null 2>&1
    sz=$(python3 -I -c "
import struct,sys
d=open('/root/out/game_${res}_${mbps}.pwv1','rb').read(); o=24; s=[]
while o<len(d):
    n=struct.unpack_from('<I',d,o)[0]; s.append(n); o+=4+n
print(sum(s)//len(s))")
    p=$(python3 -I /root/tools/psnr_y4m.py /root/frames/game_$res.y4m /root/out/game_${res}_${mbps}_dec.y4m)
    echo "$res,$mbps,$bytes,$(g 'Overall Encode'),$(g 'Overall Decode'),$(g Dequant),$(g iDWT),$(g DWT),$(g Quant),$(g Analyze),$(g Resolve),$(g Packing),$sz,\"$p\""
  done
done
