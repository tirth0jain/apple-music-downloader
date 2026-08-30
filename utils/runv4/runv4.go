package runv4

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sync/errgroup"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"main/utils/structs"

	"github.com/grafov/m3u8"
	"github.com/itouakirai/mp4ff/mp4"
	"github.com/schollz/progressbar/v3"
)

const prefetchKey = "skd://itunes.apple.com/P000000000/s1/e1"

var ErrTimeout = errors.New("response timed out")

type TimedResponseBody struct {
	timeout   time.Duration
	timer     *time.Timer
	threshold int
	body      io.Reader
}
type decryptJob struct {
	Seq       int           // 分片序号，用于重组
	Frag      *mp4.Fragment // 原始分片
	KeyURI    string        // 该分片加密使用的 EXT-X-KEY URI
	RawOffset int64
}

// 定义输出结果
type decryptResult struct {
	Seq       int
	Frag      *mp4.Fragment
	RawOffset int64
	Samples   []mp4.FullSample
}

func (b *TimedResponseBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil {
		return n, err
	}
	// fmt.Printf("Read %d bytes, buffer size %d bytes", n, len(p))
	if n >= b.threshold {
		b.timer.Reset(b.timeout)
	}
	return n, err
}

const (
	downloadMaxAttempts = 5                // 最多尝试次数
	downloadIdleTimeout = 30 * time.Second // 30 秒没收到任何字节就认为卡死

	// parallelRangeWorkers - concurrent byte-range fetches for Apple's
	// single-file HLS layout (the CDN caps a single connection at ~2.5MB/s
	// but serves ~60MB/s aggregate across 19 parallel ranges).
	parallelRangeWorkers = 12
)

// fetchRange - fetch one byte range (Retry with backoff).
func fetchRange(ctx context.Context, client *http.Client, fileUrl string, off, length int64, f *os.File) error {
	for attempt := 1; attempt <= downloadMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET", fileUrl, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
				data, rerr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if rerr == nil && int64(len(data)) == length {
					_, werr := f.WriteAt(data, off)
					if werr != nil {
						return werr
					}
					return nil
				}
				if rerr == nil {
					err = fmt.Errorf("short range at %d: got %d of %d bytes", off, len(data), length)
				} else {
					err = rerr
				}
			} else {
				err = fmt.Errorf("range at %d: server returned %s", off, resp.Status)
				resp.Body.Close()
			}
		}
		if attempt < downloadMaxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 250 * time.Millisecond):
			}
		}
	}
	return fmt.Errorf("range at %d failed after %d attempts", off, downloadMaxAttempts)
}

// downloadParallelRanges - fetch all byte-range segments (and the init
// prefix) concurrently into the preallocated file.
func downloadParallelRanges(ctx context.Context, client *http.Client, fileUrl string,
	segments []*m3u8.MediaSegment, f *os.File, totalLen int64) error {

	type rng struct{ off, length int64 }
	ranges := []rng{{0, segments[0].Offset}} // init = bytes before first segment
	sawRange := false
	for _, seg := range segments {
		if seg == nil {
			continue
		}
		if seg.Limit <= 0 || seg.Offset <= 0 {
			continue
		}
		ranges = append(ranges, rng{seg.Offset, seg.Limit})
		sawRange = true
	}
	if !sawRange {
		return errors.New("no byte ranges found in playlist")
	}
	if err := f.Truncate(totalLen); err != nil {
		return err
	}

	jobs := make(chan rng, len(ranges))
	for _, r := range ranges {
		jobs <- r
	}
	close(jobs)

	var wg sync.WaitGroup
	errCh := make(chan error, len(ranges))
	for i := 0; i < parallelRangeWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				if err := fetchRange(ctx, client, fileUrl, r.off, r.length, f); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// downloadWithResume 下载完整文件到内存，支持断点续传、空闲超时和重试。
// 只有拿到 totalLen 字节才返回成功。
func downloadWithResume(ctx context.Context, client *http.Client, fileUrl string,
	header http.Header, totalLen int64, bar *progressbar.ProgressBar) (*bytes.Buffer, error) {

	buf := &bytes.Buffer{}
	var offset int64
	backoff := 2 * time.Second

	for attempt := 1; attempt <= downloadMaxAttempts; attempt++ {
		if attempt > 1 {
			fmt.Printf("Download interrupted at %d/%d bytes, retrying in %v\n", offset, totalLen, backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}

		// 每次尝试用独立的子 context，卡死时只取消本次连接
		attemptCtx, attemptCancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(attemptCtx, "GET", fileUrl, nil)
		if err != nil {
			attemptCancel()
			return nil, err
		}
		req.Header = header
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset)) // 断点续传
		}

		resp, err := client.Do(req)
		if err != nil {
			attemptCancel()
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			attemptCancel()
			return nil, fmt.Errorf("download failed: server returned %s", resp.Status)
		}
		if offset > 0 && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			attemptCancel()
			return nil, errors.New("server does not support Range requests, cannot resume")
		}

		// 空闲超时检测：没有新数据到达就取消本次请求，触发重试
		timer := time.AfterFunc(downloadIdleTimeout, attemptCancel)
		body := &TimedResponseBody{
			timeout:   downloadIdleTimeout,
			timer:     timer,
			threshold: 1, // 只要读到字节就重置计时器
			body:      resp.Body,
		}

		n, copyErr := io.Copy(io.MultiWriter(buf, bar), body)
		resp.Body.Close()
		timer.Stop()
		attemptCancel()
		offset += n

		if copyErr == nil && offset == totalLen {
			return buf, nil // 完整拿到，才算下载成功
		}
		if copyErr == nil {
			copyErr = fmt.Errorf("short download: got %d of %d bytes", offset, totalLen)
		}
	}
	return nil, fmt.Errorf("download failed after %d attempts (got %d/%d bytes)",
		downloadMaxAttempts, offset, totalLen)
}

func Run(adamId string, playlistUrl string, outfile string, Config structs.ConfigSet) error {
	if Config.LiteServer == "" {
		return errors.New("lite-server is not configured in config.yaml")
	}
	var err error
	var optstimeout uint
	optstimeout = 0
	timeout := time.Duration(optstimeout * uint(time.Millisecond))
	header := make(http.Header)

	// request media playlist
	req, err := http.NewRequest("GET", playlistUrl, nil)
	if err != nil {
		return err
	}
	req.Header = header
	// requesting an HLS playlist should be relatively fast, so we set the timeout directly on the client
	do, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return err
	}

	// parse m3u8
	segments, err := parseMediaPlaylist(do.Body)
	if err != nil {
		return err
	}
	segment := segments[0]
	if segment == nil {
		return errors.New("no segments extracted from playlist")
	}
	if segment.Limit <= 0 {
		return errors.New("non-byterange playlists are currently unsupported")
	}

	// If the playlist carries no EXT-X-KEY (Apple sometimes omits it from the
	// web-API copy), fall back to the wrapper's OWN session playlist — the
	// /key endpoint then returns the template for the right session key.
	defaultKeyURI := ""
	if !segmentsHaveKey(segments) {
		defaultKeyURI = wrapperSessionKeyURI(Config.LiteServer, adamId)
		if defaultKeyURI != "" {
			fmt.Printf("playlist has no key line; using wrapper session key %s\n", defaultKeyURI)
		}
	}

	// get URL to the actual file
	parsedUrl, err := url.Parse(playlistUrl)
	if err != nil {
		return err
	}
	fileUrl, err := parsedUrl.Parse(segment.URI)
	if err != nil {
		return err
	}

	// request mp4
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	client := &http.Client{Timeout: timeout}

	var body io.Reader
	var totalLen int64

	if segment.Limit > 0 {
		// Apple single-file byte-range HLS: fetch every range concurrently
		// into a temp file, then decrypt+mux from it. This is the fast path
		// (parallel connections smash the ~2.5MB/s single-connection cap).
		totalLen = segment.Offset
		for _, seg := range segments {
			if seg == nil {
				continue
			}
			if seg.Limit > 0 && seg.Offset > 0 {
				end := seg.Offset + seg.Limit
				if end > totalLen {
					totalLen = end
				}
			}
		}
		tmpDir := os.Getenv("AMDL_TMPDIR")
		if tmpDir == "" {
			tmpDir = os.TempDir()
		}
		tmp, err := os.CreateTemp(tmpDir, "amdl-*.mp4")
		if err != nil {
			return err
		}
		defer tmp.Close()
		defer os.Remove(tmp.Name())
		if err := downloadParallelRanges(ctx, client, fileUrl.String(), segments, tmp, totalLen); err != nil {
			return err
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		body = tmp
		fmt.Printf("Downloaded (parallel byte ranges)\n")
	} else {
		req, err := http.NewRequestWithContext(ctx, "GET", fileUrl.String(), nil)
		if err != nil {
			return err
		}
		req.Header = make(http.Header)

		var do *http.Response
		if optstimeout > 0 {
			// create the timer before calling Do so that the timeout covers TCP handshake,
			// TLS handshake, sending the request and receiving HTTP headers
			timer := time.AfterFunc(timeout, func() { cancel(ErrTimeout) })
			do, err = client.Do(req)
			if err != nil {
				return err
			}
			defer do.Body.Close()
			body = &TimedResponseBody{
				timeout:   timeout,
				timer:     timer,
				threshold: 256,
				body:      do.Body,
			}
		} else {
			do, err = client.Do(req)
			if err != nil {
				return err
			}
			defer do.Body.Close()
			if do.ContentLength < int64(Config.MaxMemoryLimit*1024*1024) {
				bar := progressbar.NewOptions64(
					do.ContentLength,
					progressbar.OptionClearOnFinish(),
					progressbar.OptionSetElapsedTime(false),
					progressbar.OptionSetPredictTime(false),
					progressbar.OptionShowElapsedTimeOnFinish(),
					progressbar.OptionShowCount(),
					progressbar.OptionEnableColorCodes(true),
					progressbar.OptionShowBytes(true),
					progressbar.OptionSetDescription("Downloading..."),
					progressbar.OptionSetTheme(progressbar.Theme{
						Saucer:        "",
						SaucerHead:    "",
						SaucerPadding: "",
						BarStart:      "",
						BarEnd:        "",
					}),
				)
				buffer, err := downloadWithResume(ctx, client, fileUrl.String(), req.Header, do.ContentLength, bar)
				if err != nil {
					return err // 下载没完成就失败退出，绝不进入解密
				}

				body = buffer
				fmt.Print("Downloaded\n")
			} else {
				body = do.Body
			}
		}
		totalLen = do.ContentLength
	}

	err = downloadAndDecryptFile(Config.LiteServer, body, outfile, adamId, segments, totalLen, defaultKeyURI, Config)
	if err != nil {
		return err
	}
	fmt.Print("Decrypted\n")
	return nil
}

func downloadAndDecryptFile(liteServer string, in io.Reader, outfile string,
	adamId string, playlistSegments []*m3u8.MediaSegment, totalLen int64,
	defaultKeyURI string, Config structs.ConfigSet) error {
	var buffer bytes.Buffer
	var outBuf *bufio.Writer
	MaxMemorySize := int64(Config.MaxMemoryLimit * 1024 * 1024)
	inBuf := bufio.NewReader(in)
	if totalLen <= MaxMemorySize {
		outBuf = bufio.NewWriter(&buffer)
	} else {
		ofh, err := os.Create(outfile)
		if err != nil {
			return err
		}
		defer ofh.Close()
		outBuf = bufio.NewWriter(ofh)
	}
	init, offset, err := ReadInitSegment(inBuf)
	if err != nil {
		return err
	}
	if init == nil {
		return errors.New("no init segment found")
	}

	// DecryptInit mutates the init (strips sinf/pssh) — call it exactly once
	// and derive the track map from that single result.
	di, err := mp4.DecryptInit(init)
	if err != nil {
		return err
	}
	tracks := make(map[uint32]mp4.DecryptTrackInfo, len(di.TrackInfos))
	for _, ti := range di.TrackInfos {
		// tracks map: prefer the TrackInfo that carries a trex
		if prev, ok := tracks[ti.TrackID]; !ok || prev.Trex == nil {
			tracks[ti.TrackID] = ti
		}
	}
	err = sanitizeInit(init)
	if err != nil {
		// errors returned by sanitizeInit are non-fatal
		fmt.Printf("Warning: unable to sanitize init completely: %s\n", err)
	}

	// Decrypt templates per key uri, cached (thread-safe). s1/e1 uses the
	// built-in prefetch template; other uris are fetched from wrapper-lite
	// /key (ctx/state/regs) lazily on first use.
	var tmplMu sync.Mutex
	tmplCache := map[string]*template{}
	templateFor := func(uri string) (*template, error) {
		if uri == prefetchKey || uri == "" {
			return prefetchTemplate(), nil
		}
		tmplMu.Lock()
		defer tmplMu.Unlock()
		if t, ok := tmplCache[uri]; ok {
			return t, nil
		}
		t, err := fetchTemplate(liteServer, adamId, uri, Config.LiteServerToken)
		if err != nil {
			return nil, err
		}
		tmplCache[uri] = t
		return t, nil
	}

	// Output is a standard m4a (moov-first, full sample tables) assembled
	// from the decrypted fragments AFTER decryption completes. Apple's fMP4
	// layout is not standalone-decodable by ffmpeg, so we re-mux it.

	// 'segment' in m3u8 == 'fragment' in mp4ff
	//fmt.Println("Starting decryption...")
	bar := progressbar.NewOptions64(totalLen,
		//progressbar.OptionClearOnFinish(),
		progressbar.OptionSetElapsedTime(false),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionShowElapsedTimeOnFinish(),
		progressbar.OptionShowCount(),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetDescription("Decrypting..."),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "",
			SaucerHead:    "",
			SaucerPadding: "",
			BarStart:      "",
			BarEnd:        "",
		}),
	)
	bar.Add64(int64(offset))

	// 1. 引入 errgroup 和 context，实现任意错误瞬间熔断全局
	eg, ctx := errgroup.WithContext(context.Background())

	// 通道设计：任务分发通道 与 结果汇总通道
	// 缓冲区大小决定了最大可“乱序”的跨度，防止读取过快撑爆内存
	jobs := make(chan *decryptJob, 10)
	results := make(chan *decryptResult, 10)

	// 2. 启动 Writer (按序收集解密后的 samples，最后统一 re-mux 成标准 m4a)
	eg.Go(func() error {
		// 乱序重组缓冲区 (Reassembly Buffer)
		buffer := make(map[int]*decryptResult)
		expectedSeq := 0 // 期待写入的下一个序号
		var samples []mp4.FullSample

		for {
			select {
			case <-ctx.Done(): // 收到取消信号，立即退出
				return ctx.Err()
			case res, ok := <-results:
				if !ok {
					// results 通道已关闭，说明所有解密完成；re-mux 并写出
					return muxFragments(init, samples, outBuf)
				}

				// 将乱序到达的结果放入缓冲区
				buffer[res.Seq] = res

				// 检查当前期望的序号是否已经准备好，准备好就一直往前推进
				for {
					if readyRes, exists := buffer[expectedSeq]; exists {
						samples = append(samples, readyRes.Samples...)
						bar.Add64(readyRes.RawOffset)

						// 清理内存并期待下一个
						delete(buffer, expectedSeq)
						expectedSeq++
					} else {
						// 还没轮到，跳出等待
						break
					}
				}
			}
		}
	})

	// 3. 启动固定 10 个解密 Worker (乱序执行)
	var workerWg sync.WaitGroup
	for i := 0; i < 10; i++ {
		workerWg.Add(1)
		eg.Go(func() error {
			defer workerWg.Done()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case job, ok := <-jobs:
					if !ok {
						return nil // 任务分发完毕，Worker 下班
					}
					// 核心解密：wrapper-lite /key 提供每个 key uri 的模板
					// (ctx/state/regs) — s1/e1 用内置 prefetch template，其余
					// 惰性获取并缓存。samples 指向已解密的内存。
					tmpl, err := templateFor(job.KeyURI)
					if err != nil {
						return fmt.Errorf("template seq %d: %w", job.Seq, err)
					}
					samples, err := DecryptFragment(job.Frag, tracks, tmpl)
					if err != nil {
						return fmt.Errorf("tmpl decrypt seq %d: %w", job.Seq, err)
					}

					// 提交解密结果（samples 指向已解密的内存）
					select {
					case results <- &decryptResult{
						Seq:       job.Seq,
						Frag:      job.Frag,
						RawOffset: job.RawOffset,
						Samples:   samples,
					}:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
		})
	}

	// 监控所有 Worker 是否完成，完成后关闭 results 通道
	eg.Go(func() error {
		workerWg.Wait() // 等待10个工人都下班
		close(results)  // 通知 Writer 可以收尾了
		return nil
	})

	// 4. 启动 Reader (主线程负责读取)
	eg.Go(func() error {
		defer close(jobs) // 读取完毕，关闭任务通道
		seq := 0
		curKey := defaultKeyURI

		for i := 0; ; i++ {
			// 检查是否发生了全局错误，如果有则放弃读取
			if ctx.Err() != nil {
				return ctx.Err()
			}

			var frag *mp4.Fragment
			rawoffset := offset
			frag, offset, err = ReadNextFragment(inBuf, offset)
			rawoffset = offset - rawoffset
			if err != nil {
				return fmt.Errorf("read fragment: %w", err)
			}
			if frag == nil {
				break // 读到文件末尾
			}

			// 该 fragment 的加密 key uri（继承当前 EXT-X-KEY 状态：grafov 只在
			// key 行之后的第一个 segment 上挂 Key，后续 segment 需继承）
			if i < len(playlistSegments) && playlistSegments[i] != nil &&
				playlistSegments[i].Key != nil && playlistSegments[i].Key.URI != "" {
				curKey = playlistSegments[i].Key.URI
			}

			// 将任务发送给 Workers
			job := &decryptJob{
				Seq:       seq,
				Frag:      frag,
				KeyURI:    curKey,
				RawOffset: int64(rawoffset),
			}

			select {
			case jobs <- job:
			case <-ctx.Done():
				return ctx.Err()
			}
			seq++
		}
		return nil
	})

	// 5. 阻塞等待：这行代码会等待 Reader、Workers、Writer 彻底完成，或者捕获第一个发生的错误
	if err := eg.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	// ... (后续的 Flush 和 Buffer 写盘操作保持不变) ...
	err = outBuf.Flush()
	if err != nil {
		return err
	}
	if totalLen <= MaxMemorySize {
		// create output file
		ofh, err := os.Create(outfile)
		if err != nil {
			return err
		}
		defer ofh.Close()

		_, err = ofh.Write(buffer.Bytes())
		if err != nil {
			return err
		}
	}
	return nil
}

// Remove boxes in the init segment that are known to cause compatibility issues
func sanitizeInit(init *mp4.InitSegment) error {
	traks := init.Moov.Traks
	if len(traks) > 1 {
		return errors.New("more than 1 track found")
	}
	// Remove duplicate ec-3 or alac boxes in stsd since some programs (e.g. cuetools) don't
	// like it when there's more than 1 entry in stsd.
	// Every audio track contains two of these boxes because two IVs are needed to decrypt the
	// track. The two boxes become identical after removing encryption info.
	stsd := traks[0].Mdia.Minf.Stbl.Stsd
	if stsd.SampleCount == 1 {
		return nil
	}
	if stsd.SampleCount > 2 {
		return fmt.Errorf("expected only 1 or 2 entries in stsd, got %d", stsd.SampleCount)
	}
	children := stsd.Children
	if children[0].Type() != children[1].Type() {
		return errors.New("children in stsd are not of the same type")
	}
	stsd.Children = children[:1]
	stsd.SampleCount = 1
	return nil
}

// Workaround for m3u8 not supporting multiple keys - remove
// PlayReady and Widevine
func filterResponse(f io.Reader) (*bytes.Buffer, error) {
	buf := &bytes.Buffer{}
	scanner := bufio.NewScanner(f)

	prefix := []byte("#EXT-X-KEY:")
	keyFormat := []byte("streamingkeydelivery")
	for scanner.Scan() {
		lineBytes := scanner.Bytes()
		if bytes.HasPrefix(lineBytes, prefix) && !bytes.Contains(lineBytes, keyFormat) {
			continue
		}
		_, err := buf.Write(lineBytes)
		if err != nil {
			return nil, err
		}
		_, err = buf.WriteString("\n")
		if err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return buf, nil
}

func parseMediaPlaylist(r io.ReadCloser) ([]*m3u8.MediaSegment, error) {
	defer r.Close()
	playlistBuf, err := filterResponse(r)
	if err != nil {
		return nil, err
	}

	playlist, listType, err := m3u8.Decode(*playlistBuf, true)
	if err != nil {
		return nil, err
	}

	if listType != m3u8.MEDIA {
		return nil, errors.New("m3u8 not of media type")
	}

	mediaPlaylist := playlist.(*m3u8.MediaPlaylist)
	return mediaPlaylist.Segments, nil
}

// pasing
func ReadInitSegment(r io.Reader) (*mp4.InitSegment, uint64, error) {
	var offset uint64 = 0
	init := mp4.NewMP4Init()
	for i := 0; i < 2; i++ {
		box, err := mp4.DecodeBox(offset, r)
		if err != nil {
			return nil, offset, err
		}
		boxType := box.Type()
		if boxType != "ftyp" && boxType != "moov" {
			return nil, offset, fmt.Errorf("unexpected box type %s, should be ftyp or moov", boxType)
		}
		init.AddChild(box)
		offset += box.Size()
	}
	return init, offset, nil
}

// Get the next fragment. Returns nil and no error on EOF
func ReadNextFragment(r io.Reader, offset uint64) (*mp4.Fragment, uint64, error) {
	frag := mp4.NewFragment()
	for {
		box, err := mp4.DecodeBox(offset, r)
		if err == io.EOF {
			return nil, offset, nil
		}
		if err != nil {
			return nil, offset, err
		}
		boxType := box.Type()
		// fmt.Printf("processing %s, box starts @ offset %d\n", boxType, offset)
		offset += box.Size()
		if boxType == "moof" || boxType == "emsg" || boxType == "prft" {
			frag.AddChild(box)
			continue
		}
		if boxType == "mdat" {
			frag.AddChild(box)
			break
		}
		fmt.Printf("ignoring a %s box found mid-stream", boxType)
	}
	// only 1 mdat box in fragment, meaning that the box doesn't have a preceding moof box
	if frag.Moof == nil {
		return nil, offset, fmt.Errorf("more than one mdat box in fragment (box ends @ offset %d)", offset)
	}
	return frag, offset, nil
}

// Return a new slice of boxes with encryption-related sbgp and sgpd removed,
// and the total number of bytes removed.
// Non-encryption-related ones such as 'roll' are left untouched.
func FilterSbgpSgpd(children []mp4.Box) ([]mp4.Box, uint64) {
	var bytesRemoved uint64 = 0
	remainingChildren := make([]mp4.Box, 0, len(children))
	for _, child := range children {
		switch box := child.(type) {
		case *mp4.SbgpBox:
			if box.GroupingType == "seam" || box.GroupingType == "seig" {
				bytesRemoved += child.Size()
				continue
			}
		case *mp4.SgpdBox:
			if box.GroupingType == "seam" || box.GroupingType == "seig" {
				bytesRemoved += child.Size()
				continue
			}
		}
		remainingChildren = append(remainingChildren, child)
	}
	return remainingChildren, bytesRemoved
}

// requestContentKey - acquire the track's content key via lite-server
// muxFragments - write a standard m4a from the ordered decrypted samples.
func muxFragments(init *mp4.InitSegment, samples []mp4.FullSample, w *bufio.Writer) error {
	if len(samples) == 0 {
		return errors.New("no samples to mux")
	}
	if err := MuxStandardM4A(init, samples, w); err != nil {
		return err
	}
	return nil
}

// Get decryption info for tracks from init segment and remove encryption-related boxes
func TransformInit(init *mp4.InitSegment) (map[uint32]mp4.DecryptTrackInfo, error) {
	di, err := mp4.DecryptInit(init)
	tracks := make(map[uint32]mp4.DecryptTrackInfo, len(di.TrackInfos))
	for _, ti := range di.TrackInfos {
		tracks[ti.TrackID] = ti
	}
	if err != nil {
		return tracks, err
	}
	// remove encryption-related sbgp and sgpd
	for _, trak := range init.Moov.Traks {
		stbl := trak.Mdia.Minf.Stbl
		stbl.Children, _ = FilterSbgpSgpd(stbl.Children)
	}
	return tracks, nil
}

// Decryption function dispatcher
func cbcsDecryptRaw(data []byte, decryptBlockLen, skipBlockLen int, tmpl *template) error {
	if skipBlockLen != 0 {
		return fmt.Errorf("not full encryption of subsamples")
	}
	// Drops 4 last bits -> multiple of 16
	// It wouldn't hurt to send the remaining bytes also because the decryption
	// function would just return them as-is, but we're truncating the data here
	// for clarity and interoperability
	truncatedLen := len(data) & ^0xf
	decrypted := decryptSample(tmpl, data[:truncatedLen])
	copy(data[:truncatedLen], decrypted)
	// Full encryption of subsamples
	// e.g. Apple Music ALAC
	return nil
}

// Decrypt a cbcs-encrypted sample in-place
func cbcsDecryptSample(sample []byte, subSamplePatterns []mp4.SubSamplePattern, tenc *mp4.TencBox, tmpl *template) error {

	decryptBlockLen := int(tenc.DefaultCryptByteBlock) * 16
	skipBlockLen := int(tenc.DefaultSkipByteBlock) * 16
	var pos uint32 = 0

	// Full sample encryption
	if len(subSamplePatterns) == 0 {
		return cbcsDecryptRaw(sample, decryptBlockLen, skipBlockLen, tmpl)
	}

	// Has subsamples
	for j := 0; j < len(subSamplePatterns); j++ {
		ss := subSamplePatterns[j]
		pos += uint32(ss.BytesOfClearData)

		// Nothing to decrypt!
		if ss.BytesOfProtectedData <= 0 {
			continue
		}

		err := cbcsDecryptRaw(sample[pos:pos+ss.BytesOfProtectedData], decryptBlockLen, skipBlockLen, tmpl)
		if err != nil {
			return err
		}
		pos += ss.BytesOfProtectedData
	}

	return nil
}

// Decrypt an array of cbcs-encrypted samples in-place
func cbcsDecryptSamples(samples []mp4.FullSample, tmpl *template,
	tenc *mp4.TencBox, senc *mp4.SencBox) error {

	for i := range samples {
		var subSamplePatterns []mp4.SubSamplePattern
		if len(senc.SubSamples) != 0 {
			subSamplePatterns = senc.SubSamples[i]
		}
		err := cbcsDecryptSample(samples[i].Data, subSamplePatterns, tenc, tmpl)
		if err != nil {
			return err
		}
	}
	return nil
}

// Decrypt a cbcs-encrypted sample in-place
func DecryptFragment(frag *mp4.Fragment, tracks map[uint32]mp4.DecryptTrackInfo, tmpl *template) ([]mp4.FullSample, error) {
	moof := frag.Moof
	var bytesRemoved uint64 = 0
	var allSamples []mp4.FullSample

	for _, traf := range moof.Trafs {
		ti, ok := tracks[traf.Tfhd.TrackID]
		if !ok {
			return nil, fmt.Errorf("could not find decryption info for track %d", traf.Tfhd.TrackID)
		}
		if ti.Sinf == nil {
			// unencrypted track
			continue
		}

		schemeType := ti.Sinf.Schm.SchemeType
		if schemeType != "cbcs" {
			return nil, fmt.Errorf("scheme type %s not supported", schemeType)
		}
		hasSenc, isParsed := traf.ContainsSencBox()
		if !hasSenc {
			return nil, fmt.Errorf("no senc box in traf")
		}

		var senc *mp4.SencBox
		if traf.Senc != nil {
			senc = traf.Senc
		} else {
			senc = traf.UUIDSenc.Senc
		}

		if !isParsed {
			// simply ignore sbgp and sgpd
			// "Sample To Group Box ('sbgp') and Sample Group Description Box ('sgpd')
			// of type 'seig' are used to indicate the KID applied to each sample, and changes
			// to KIDs over time (i.e. 'key rotation')"
			// (ref: https://dashif.org/docs/DASH-IF-IOP-v3.2.pdf)
			err := senc.ParseReadBox(ti.Sinf.Schi.Tenc.DefaultPerSampleIVSize, traf.Saiz)
			if err != nil {
				return nil, err
			}
		}

		samples, err := frag.GetFullSamples(ti.Trex)
		if err != nil {
			return nil, err
		}

		err = cbcsDecryptSamples(samples, tmpl, ti.Sinf.Schi.Tenc, senc)
		if err != nil {
			return nil, err
		}

		allSamples = append(allSamples, samples...)
		bytesRemoved += traf.RemoveEncryptionBoxes()
	}
	_, psshBytesRemoved := moof.RemovePsshs()
	bytesRemoved += psshBytesRemoved
	for _, traf := range moof.Trafs {
		for _, trun := range traf.Truns {
			trun.DataOffset -= int32(bytesRemoved)
		}
	}

	return allSamples, nil
}

// segmentsHaveKey - does any playlist segment carry an EXT-X-KEY uri?
func segmentsHaveKey(segs []*m3u8.MediaSegment) bool {
	for _, seg := range segs {
		if seg != nil && seg.Key != nil && seg.Key.URI != "" {
			return true
		}
	}
	return false
}

// wrapperSessionKeyURI - fetch the wrapper's own session playlist for the
// adamId and return its (last) EXT-X-KEY uri, or "" on any failure.
func wrapperSessionKeyURI(liteServer, adamId string) string {
	if liteServer == "" {
		return ""
	}
	endpoint := strings.TrimRight(liteServer, "/") + "/m3u8?adamId=" + url.QueryEscape(adamId)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := http.Get(endpoint)
		if err == nil {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil {
				var env struct {
					Code int    `json:"code"`
					Msg  string `json:"msg"`
					Data struct {
						M3u8 string `json:"m3u8"`
					} `json:"data"`
				}
				if json.NewDecoder(bytes.NewReader(body)).Decode(&env) == nil && env.Code == 0 && env.Data.M3u8 != "" {
					if uri := mediaPlaylistKeyURI(env.Data.M3u8); uri != "" {
						return uri
					}
				}
			}
		} else {
			lastErr = err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	_ = lastErr
	return ""
}

// mediaPlaylistKeyURI - fetch + parse an HLS media playlist, return the last
// EXT-X-KEY uri found ("" if none / failure).
func mediaPlaylistKeyURI(playlistURL string) string {
	resp, err := http.Get(playlistURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	from, listType, err := m3u8.DecodeFrom(resp.Body, true)
	if err != nil || listType != m3u8.MEDIA {
		return ""
	}
	media := from.(*m3u8.MediaPlaylist)
	if media.Key != nil && media.Key.URI != "" {
		return strings.Split(media.Key.URI, ",")[0]
	}
	return ""
}
