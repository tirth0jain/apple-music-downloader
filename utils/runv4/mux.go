package runv4

// Standard-m4a muxer: converts Apple's fragment layout (byte-range single-file
// HLS, which ffmpeg cannot decode standalone) into a normal moov-first m4a
// with full sample tables. The ALAC sample entry (2ch/24-bit/44100 frame
// 4096) is taken verbatim from an ffmpeg -c:a alac encode so that ffmpeg's
// demuxer+decoder accept it; rate/channels/depth fields are patched from the
// source track's stsd.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/Eyevinn/mp4ff/bits"
	"github.com/itouakirai/mp4ff/mp4"
)

// fixedBox: generic box with raw payload (stand-in for boxes mp4ff lacks).
type fixedBox struct {
	typ     string
	payload []byte
}

func (b *fixedBox) Type() string { return b.typ }
func (b *fixedBox) Size() uint64 { return uint64(8 + len(b.payload)) }
func (b *fixedBox) Encode(w io.Writer) error {
	if err := mp4.EncodeHeader(b, w); err != nil {
		return err
	}
	_, err := w.Write(b.payload)
	return err
}
func (b *fixedBox) EncodeSW(sw bits.SliceWriter) error {
	if err := mp4.EncodeHeaderSW(b, sw); err != nil {
		return err
	}
	sw.WriteBytes(b.payload)
	return nil
}
func (b *fixedBox) Info(w io.Writer, _, _, _ string) error {
	_, err := fmt.Fprintf(w, "%s: %d bytes\n", b.typ, b.Size())
	return err
}

// alacEntryBody: 64 bytes after the sample-entry header, from an ffmpeg
// -c:a alac encode (2ch/24-bit/44100, frameLength 4096).
// Layout: [0:4] verflags, [4:6] res, [6:8] dataref, [8:16] res,
// [16:18] channelcount, [18:20] samplesize, [20:24] pre+res,
// [24:28] samplerate (16.16), [28:64] child 'alac' box with cookie;
// cookie at [40:64], cookie sampleRate at [60:64].
var alacEntryBody = []byte{
	0x00, 0x00, 0x00, 0x00, // version/flags
	0x00, 0x00, 0x00, 0x01, // reserved(2)+dataref(2)
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // reserved(8)
	0x00, 0x02, 0x00, 0x18, // channelcount=2, samplesize=24
	0x00, 0x00, 0x00, 0x00, // pre_defined+reserved
	0x00, 0x00, 0xac, 0x44, // samplerate 44100<<16
	0x00, 0x00, 0x00, 0x24, 0x61, 0x6c, 0x61, 0x63, // child: size 36, 'alac'
	0x00, 0x00, 0x00, 0x00, // cookie version/flags
	0x00, 0x00, 0x10, 0x00, // frameLength 4096
	0x00, 0x18, 0x28, 0x0a, // version 0, bitDepth 24, pb, mb
	0x0e, 0x02, 0x00, 0xff, // numChannels ... (0xff per gamdl's real cookie)
	0x00, 0x00, 0x60, 0x04, // maxRun
	0x00, 0x20, 0x4c, 0xc0, // maxFrameBytes
	0x00, 0x00, 0xac, 0x44, // sampleRate 44100
}

// patchAudioParams - set channelcount, samplesize and samplerate in both the
// entry fields and the ALAC magic cookie. The AudioSampleEntry samplerate is
// 16.16 fixed-point (44100 << 16); the magic cookie stores it raw.
// patchRateOnly - normalize the samplerate fields (entry 16.16 field +
// magic-cookie raw rate) AND force a sane channel count, leaving the rest of
// Apple's cookie bytes untouched. Apple's hires init occasionally carries a
// cookie whose channel byte reads as 0x10 (16) for ordinary stereo masters;
// ffmpeg then refuses to decode ("Channel count 16 is not implemented") and
// the FLAC transcode produces a corrupt header. The actual ALAC frames are
// stereo, so normalizing to 2 is always safe.
func patchRateOnly(body []byte, rate uint32) []byte {
	out := make([]byte, len(body))
	copy(out, body)
	rateFixed := rate << 16
	out[24] = byte(rateFixed >> 24)
	out[25] = byte(rateFixed >> 16)
	out[26] = byte(rateFixed >> 8)
	out[27] = byte(rateFixed)
	out[60] = byte(rate >> 24)
	out[61] = byte(rate >> 16)
	out[62] = byte(rate >> 8)
	out[63] = byte(rate)
	// entry channelcount at body[16:18] (big-endian uint16) — force stereo.
	out[16], out[17] = 0x00, 0x02
	// cookie numChannels byte: cookie starts at body[36]; per Apple's
	// ALACSpecificConfig, numChannels is cookie[9] = body[45]. The sample
	// entry's channelcount at [16:18] is authoritative — mirror it.
	if len(out) > 45 {
		out[45] = out[17]
	}
	return out
}

func patchAudioParams(body []byte, channels uint16, sampleSize uint16, rate uint32) []byte {
	out := make([]byte, len(body))
	copy(out, body)
	out[16] = byte(channels >> 8)
	out[17] = byte(channels)
	out[18] = byte(sampleSize >> 8)
	out[19] = byte(sampleSize)
	// magic-cookie bitDepth byte (body[45] = entry[53])
	out[45] = byte(sampleSize)
	rateFixed := rate << 16
	out[24] = byte(rateFixed >> 24)
	out[25] = byte(rateFixed >> 16)
	out[26] = byte(rateFixed >> 8)
	out[27] = byte(rateFixed)
	out[60] = byte(rate >> 24)
	out[61] = byte(rate >> 16)
	out[62] = byte(rate >> 8)
	out[63] = byte(rate)
	return out
}

// MuxStandardM4A - write a standard m4a (ftyp + moov + mdat) from decrypted
// fragment samples. Audio parameters come from the source stsd entry.
func MuxStandardM4A(init *mp4.InitSegment, samples []mp4.FullSample, w io.Writer) error {
	if len(samples) == 0 {
		return fmt.Errorf("no samples to mux")
	}

	// audio params from the original stsd entry bytes
	trak := init.Moov.Traks[0]
	stsd := trak.Mdia.Minf.Stbl.Stsd
	channels, sampleSize := uint16(2), uint16(24)
	sampleRate := uint32(44100)
	var srcEntry []byte // the source's own alac sample entry (with Apple's cookie)
	if len(stsd.Children) > 0 {
		var buf bytes.Buffer
		_ = stsd.Children[0].Encode(&buf)
		b := buf.Bytes()
		if len(b) >= 36 {
			channels = binary.BigEndian.Uint16(b[24:26])
			sampleSize = binary.BigEndian.Uint16(b[26:28])
			sampleRate = binary.BigEndian.Uint32(b[32:36]) >> 16
		}
		if len(b) >= 72 && string(b[4:8]) == "alac" {
			srcEntry = b
		}
	}
	if sampleRate == 0 {
		sampleRate = 44100
	}

	var totalDur uint64
	for _, s := range samples {
		totalDur += uint64(s.Dur)
	}

	// ---- build moov ----
	mvhd := mp4.CreateMvhd()
	mvhd.Timescale = sampleRate
	mvhd.Duration = totalDur
	mvhd.NextTrackID = 2

	track := mp4.CreateEmptyTrak(1, sampleRate, "audio", "eng")
	if track.Tkhd != nil {
		track.Tkhd.Volume = 0x0100
		track.Tkhd.Duration = totalDur
	}
	if track.Mdia != nil && track.Mdia.Mdhd != nil {
		track.Mdia.Mdhd.Duration = totalDur
	}

	stbl := track.Mdia.Minf.Stbl
	stsdOut := stbl.Stsd
	stsdOut.SampleCount = 1
	var entryBody []byte
	if srcEntry != nil {
		// Use Apple's OWN sample entry verbatim except the samplerate fields:
		// its magic cookie is authoritative for bit depth / channels /
		// maxRun / maxFrameBytes (the entry's samplesize field lies — 16 for
		// both 16- and 24-bit tracks).
		entryBody = patchRateOnly(srcEntry[8:], sampleRate)
	} else {
		entryBody = patchAudioParams(alacEntryBody, channels, sampleSize, sampleRate)
	}
	stsdOut.Children = []mp4.Box{
		&fixedBox{typ: "alac", payload: entryBody},
	}

	stts := &mp4.SttsBox{}
	var runCount, runDelta []uint32
	for _, s := range samples {
		if len(runDelta) == 0 || runDelta[len(runDelta)-1] != s.Dur {
			runCount = append(runCount, 1)
			runDelta = append(runDelta, s.Dur)
		} else {
			runCount[len(runCount)-1]++
		}
	}
	stts.SampleCount = runCount
	stts.SampleTimeDelta = runDelta

	stsc := &mp4.StscBox{}
	stsc.Entries = []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: uint32(len(samples)), FirstSampleNr: 0}}
	stsc.SampleDescriptionID = []uint32{1}

	stsz := &mp4.StszBox{}
	stsz.SampleNumber = uint32(len(samples))
	for _, s := range samples {
		stsz.SampleSize = append(stsz.SampleSize, uint32(s.Size))
	}

	stco := &mp4.StcoBox{}
	stco.ChunkOffset = []uint32{0}

	stbl.Children = nil
	stbl.Children = append(stbl.Children, stsdOut, stts, stsc, stsz, stco)

	moov := &mp4.MoovBox{}
	moov.AddChild(mvhd)
	moov.AddChild(track)

	var moovBuf bytes.Buffer
	if err := moov.Encode(&moovBuf); err != nil {
		return err
	}
	// ftyp: standard M4A brands (the source init's "iso5" brand makes the
	// fork's tagger refuse the file: "unsupported ftyp: 69736f35")
	ftyp := mp4.NewFtyp("M4A ", 0, []string{"isom", "iso2", "mp41"})
	mdatPayload := uint32(ftyp.Size() + uint64(moovBuf.Len()) + 8)
	stco.ChunkOffset[0] = mdatPayload

	moovBuf.Reset()
	if err := moov.Encode(&moovBuf); err != nil {
		return err
	}

	if err := ftyp.Encode(w); err != nil {
		return err
	}
	if _, err := w.Write(moovBuf.Bytes()); err != nil {
		return err
	}

	var mdatSize uint64 = 8
	for _, s := range samples {
		mdatSize += uint64(s.Size)
	}
	if mdatSize > (1<<32)-1 {
		return fmt.Errorf("mdat too large for 32-bit size: %d", mdatSize)
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr[0:4], uint32(mdatSize))
	copy(hdr[4:], "mdat")
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	for _, s := range samples {
		if _, err := w.Write(s.Data); err != nil {
			return err
		}
	}
	return nil
}