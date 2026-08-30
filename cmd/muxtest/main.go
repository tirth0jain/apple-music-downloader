// muxtest: prototype for re-muxing Apple's fragmented ALAC (byte-range HLS
// single-file layout) into a standard m4a (moov-first, full sample tables).
// Works on the raw (or decrypted) file; validates the muxer independently
// of decryption.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/itouakirai/mp4ff/mp4"
	"github.com/Eyevinn/mp4ff/bits"

	"main/utils/runv4"
)

// fixedBox: stand-in for boxes mp4ff has no struct for (the ALAC sample entry).
type fixedBox struct {
	typ     string
	payload []byte
}

func (b *fixedBox) Type() string        { return b.typ }
func (b *fixedBox) Size() uint64        { return uint64(8 + len(b.payload)) }
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
func (b *fixedBox) Info(w io.Writer, specificBoxLevels, indent, indentStep string) error {
	_, err := fmt.Fprintf(w, "%s%s: %d bytes\n", indent, b.typ, b.Size())
	return err
}

// reference ALAC sample entry (2ch / 24-bit / 44100, frameLength 4096),
// taken verbatim from an ffmpeg -c:a alac encod e (guaranteed self-consistent
// with ffmpeg's demuxer+decoder). Fields patched in code for other rates.
var refAlacEntry = []byte{
	0x00, 0x00, 0x00, 0x48, 0x61, 0x6c, 0x61, 0x63, // size, 'alac'
	0x00, 0x00, 0x00, 0x00, // version/flags
	0x00, 0x00, 0x00, 0x01, // reserved(2)+dataref(2)
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // reserved(8)
	0x00, 0x02, 0x00, 0x18, // channelcount=2, samplesize=24
	0x00, 0x00, 0x00, 0x00, // pre_defined+reserved
	0x00, 0x00, 0xac, 0x44, // samplerate 44100<<16
	0x00, 0x00, 0x00, 0x24, 0x61, 0x6c, 0x61, 0x63, // child: size, 'alac'
	0x00, 0x00, 0x00, 0x00, // cookie version/flags
	0x00, 0x00, 0x10, 0x00, // frameLength 4096
	0x00, 0x18, 0x28, 0x0a, // version 0, bitDepth 24, pb, mb
	0x0e, 0x02, 0x00, 0x00, // numChannels (little-ish)
	0x00, 0x00, 0x60, 0x04, // maxRun
	0x00, 0x20, 0x4c, 0xc0, // maxFrameBytes
	0x00, 0x00, 0xac, 0x44, // sampleRate 44100
}

func patchSampleRate(body []byte, rate uint32) []byte {
	// body = the 64 bytes after the sample-entry header:
	//   [0:4] verflags, [4:6] res, [6:8] dataref, [8:16] res,
	//   [16:18] channelcount, [18:20] samplesize, [20:24] pre+res,
	//   [24:28] samplerate (16.16), [28:64] child 'alac' box,
	//   cookie at body[40:64], sampleRate at body[60:64]
	out := make([]byte, len(body))
	copy(out, body)
	out[24] = byte(rate >> 24)
	out[25] = byte(rate >> 16)
	out[26] = byte(rate >> 8)
	out[27] = byte(rate)
	out[60] = byte(rate >> 24)
	out[61] = byte(rate >> 16)
	out[62] = byte(rate >> 8)
	out[63] = byte(rate)
	return out
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: muxtest <in.raw-or-decrypted> <out.m4a>")
		os.Exit(1)
	}
	inPath, outPath := os.Args[1], os.Args[2]

	f, err := os.Open(inPath)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer f.Close()
	br := bufio.NewReader(f)

	// read init (ftyp + moov)
	init, off, err := runv4.ReadInitSegment(br)
	if err != nil {
		fmt.Println("init:", err)
		os.Exit(1)
	}

	// track info (trex) via mp4ff
	di, err := mp4.DecryptInit(init)
	if err != nil {
		fmt.Println("decryptinit:", err)
		os.Exit(1)
	}
	for i, ti := range di.TrackInfos {
		defDur := uint32(0)
		if ti.Trex != nil {
			defDur = ti.Trex.DefaultSampleDuration
		}
		fmt.Printf("trackinfo %d: id=%d trexDefaultDur=%d\n", i, ti.TrackID, defDur)
	}
	if len(di.TrackInfos) == 0 {
		fmt.Println("no track infos")
		os.Exit(1)
	}
	// pick the track that appears in the fragments (tfhd), else the first
	ti := di.TrackInfos[0]

	// read all fragments, collect samples (sizes, durations, data)
	var samples []mp4.FullSample
	var totalDur uint64
	fragCount := 0
	for {
		frag, newOff, err := runv4.ReadNextFragment(br, off)
		if err != nil {
			fmt.Println("fragment:", err)
			os.Exit(1)
		}
		if frag == nil {
			break
		}
		off = newOff
		fragCount++
		fragSamples, err := frag.GetFullSamples(ti.Trex)
		if err != nil {
			fmt.Println("GetFullSamples:", err)
			os.Exit(1)
		}
		for i := range fragSamples {
			samples = append(samples, fragSamples[i])
			totalDur += uint64(fragSamples[i].Dur)
		}
	}
	fmt.Printf("fragments=%d samples=%d totalDur=%d (%.3fs)\n",
		fragCount, len(samples), totalDur, float64(totalDur)/44100)

	if len(samples) == 0 {
		fmt.Println("no samples")
		os.Exit(1)
	}

	// sample rate from fragment data is 44100 for this track; derive from
	// sample entry of the original moov if needed later.
	sampleRate := uint32(44100)

	// ---- build a fresh standard moov ----
	mvhd := mp4.CreateMvhd()
	mvhd.Timescale = sampleRate
	mvhd.Duration = totalDur
	mvhd.NextTrackID = 2

	trak := mp4.CreateEmptyTrak(1, sampleRate, "soun", "eng")
	if trak.Tkhd != nil {
		trak.Tkhd.Volume = 0x0100
		trak.Tkhd.Duration = totalDur
	}
	if trak.Mdia != nil && trak.Mdia.Mdhd != nil {
		trak.Mdia.Mdhd.Duration = totalDur
	}

	stbl := trak.Mdia.Minf.Stbl
	// stsd: replace children with our fixed ALAC entry
	stsd := stbl.Stsd
	stsd.SampleCount = 1
	stsd.Children = []mp4.Box{&fixedBox{typ: "alac", payload: patchSampleRate(refAlacEntry[8:], sampleRate)}}

	// stts: runs of equal durations
	stts := &mp4.SttsBox{}
	runCount := []uint32{}
	runDelta := []uint32{}
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
	stsz.SampleUniformSize = 0

	stco := &mp4.StcoBox{}
	stco.ChunkOffset = []uint32{0} // patched after moov size known

	// replace stbl children
	stbl.Children = nil
	stbl.Children = append(stbl.Children, stsd, stts, stsc, stsz, stco)

	moov := &mp4.MoovBox{}
	moov.AddChild(mvhd)
	moov.AddChild(trak)

	// ftyp: reuse original
	ftyp := init.Ftyp

	// encode moov to measure size
	var moovBuf bytes.Buffer
	if err := moov.Encode(&moovBuf); err != nil {
		fmt.Println("moov encode:", err)
		os.Exit(1)
	}
	moovSize := uint64(moovBuf.Len())
	ftypSize := ftyp.Size()
	mdatPayload := uint32(8 + ftypSize + moovSize + 8)
	stco.ChunkOffset[0] = mdatPayload

	// re-encode (same size)
	moovBuf.Reset()
	if err := moov.Encode(&moovBuf); err != nil {
		fmt.Println("moov re-encode:", err)
		os.Exit(1)
	}

	out, err := os.Create(outPath)
	if err != nil {
		fmt.Println("create:", err)
		os.Exit(1)
	}
	defer out.Close()
	bw := bufio.NewWriterSize(out, 1<<20)

	if err := ftyp.Encode(bw); err != nil {
		fmt.Println("ftyp:", err)
		os.Exit(1)
	}
	if _, err := bw.Write(moovBuf.Bytes()); err != nil {
		fmt.Println("moov write:", err)
		os.Exit(1)
	}
	// mdat
	var mdatSize uint64 = 8
	for _, s := range samples {
		mdatSize += uint64(s.Size)
	}
	// write header via fixed box to respect large sizes
	mdat := &fixedBox{typ: "mdat", payload: nil}
	_ = mdat
	if mdatSize <= (1<<32)-1 {
		hdr := make([]byte, 8)
		hdr[0] = byte(mdatSize >> 24)
		hdr[1] = byte(mdatSize >> 16)
		hdr[2] = byte(mdatSize >> 8)
		hdr[3] = byte(mdatSize)
		copy(hdr[4:], "mdat")
		if _, err := bw.Write(hdr); err != nil {
			fmt.Println("mdat hdr:", err)
			os.Exit(1)
		}
	}
	for _, s := range samples {
		if _, err := bw.Write(s.Data); err != nil {
			fmt.Println("mdat data:", err)
			os.Exit(1)
		}
	}
	if err := bw.Flush(); err != nil {
		fmt.Println("flush:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes, %d samples)\n", outPath, mdatSize+ftypSize+moovSize, len(samples))
}