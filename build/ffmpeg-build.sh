#!/bin/sh
# Builds the minimal static ffmpeg that ships inside vidc.
# See third_party/ffmpeg/BUILD.md for the full rationale.
set -e

FFMPEG_VERSION=9.0.1
X264_VERSION=b35605a          # pin to the revision matching the tested build
VMAF_VERSION=v3.2.0

PREFIX="$(pwd)/dist-ffmpeg"

# --- x264 (static; H.264 encoder) ---
# NOTE: `git clone --branch <sha>` does not work (a raw SHA is not a branch);
# clone then check out the pinned revision instead. The pin is unchanged.
git clone https://code.videolan.org/videolan/x264.git
cd x264
git checkout "$X264_VERSION"
./configure --prefix="$PREFIX" --enable-static --disable-cli --disable-opencl --disable-lavf
make -j"$(getconf _NPROCESSORS_ONLN)"
make install
cd ..

# --- libvmaf (static; quality metric) ---
git clone --depth 1 --branch "$VMAF_VERSION" https://github.com/Netflix/vmaf.git
cd vmaf/libvmaf
# NOTE: -Dlibdir=lib keeps libvmaf.pc under $PREFIX/lib/pkgconfig on every
# platform (meson defaults to lib/<triplet> on Debian/Ubuntu, which ffmpeg's
# PKG_CONFIG_PATH below would miss -> "libvmaf not found").
meson setup build --prefix="$PREFIX" -Dlibdir=lib --buildtype release -Ddefault_library=static
ninja -C build
ninja -C build install
cd ../..

# --- ffmpeg ---
curl -fsSLO "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz"
tar xf "ffmpeg-${FFMPEG_VERSION}.tar.xz"
cd "ffmpeg-${FFMPEG_VERSION}"

PKG_CONFIG_PATH="$PREFIX/lib/pkgconfig" ./configure \
  --prefix="$PREFIX" \
  --disable-everything \
  --disable-autodetect \
  --disable-network \
  --disable-doc --disable-htmlpages --disable-manpages --disable-podpages --disable-txtpages \
  --disable-debug --disable-stripping \
  --enable-static --disable-shared \
  --enable-gpl --enable-libx264 --enable-libvmaf \
  --enable-encoder=libx264,aac,wrapped_avframe \
  --enable-decoder=h264,hevc,mpeg4,mpeg2video,vp8,vp9,av1,mjpeg,aac,mp3,opus,vorbis,flac,ac3,eac3,alac,pcm_s16le,pcm_s24le,pcm_u8 \
  --enable-muxer=mp4,mov,null \
  --enable-demuxer=mov,matroska,avi,mpegts,flv,mp3,wav,aac,flac,ogg \
  --enable-parser=h264,hevc,mpeg4video,mpegvideo,vp8,vp9,av1,aac,mp3,opus,vorbis,flac,ac3 \
  --enable-filter=setpts,transpose,format,scale,null,trim,atrim,aresample,anull,settb,libvmaf \
  --enable-protocol=file,pipe \
  --enable-swscale --enable-swresample
make -j"$(getconf _NPROCESSORS_ONLN)"
make install
