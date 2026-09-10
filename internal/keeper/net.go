package keeper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/servekit/oracle-keeper/pkg/config"
)

// minChunkB is the smallest planned download; sub-MiB tail chunks are not
// worth a request.
const minChunkB = 1 << 20

// source is one download origin. A source with approxB == 0 is
// "parametrized": its URL contains a single %d verb replaced by the exact
// requested byte count (e.g. Cloudflare's speed endpoint). Fixed sources are
// capped server-side via the Range header and reader-side via io.CopyN.
type source struct {
	name    string
	url     string
	approxB int64
}

// builtinSources lists the default origins, verified reachable 2026-09-10.
// Real package/artifact CDNs (node tarball, npm tgz, Alpine rootfs, Go source
// tarball) keep traffic indistinguishable from routine provisioning, plus two
// endpoints designed to absorb arbitrary download volume (Cloudflare speed,
// OVH proof).
func builtinSources() []source {
	srcs := []source{
		{name: "cloudflare-speed", url: "https://speed.cloudflare.com/__down?bytes=%d"},
		{name: "ovh-proof", url: "https://proof.ovh.net/files/100Mb.dat", approxB: 13 << 20},
		{name: "codeload-github", url: "https://codeload.github.com/golang/go/tar.gz/refs/tags/go1.23.4", approxB: 24 << 20},
		{name: "npmjs", url: "https://registry.npmjs.org/typescript/-/typescript-5.7.2.tgz", approxB: 8 << 20},
	}
	if runtime.GOARCH == "arm64" || runtime.GOARCH == "amd64" {
		srcs = append(srcs, source{
			name:    "nodejs-org",
			url:     fmt.Sprintf("https://nodejs.org/dist/v22.14.0/node-v22.14.0-linux-%s.tar.xz", runtime.GOARCH),
			approxB: 22 << 20,
		})
	}
	if arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]; arch != "" {
		srcs = append(srcs, source{
			name:    "alpine-cdn",
			url:     fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/%s/alpine-minirootfs-3.21.3-%s.tar.gz", arch, arch),
			approxB: 4 << 20,
		})
	}
	return srcs
}

// parseSources builds a source list from "name=url" strings (the
// net.sources override). A %d in the URL marks a parametrized source.
func parseSources(specs []string) ([]source, error) {
	srcs := make([]source, 0, len(specs))
	for _, spec := range specs {
		name, url, ok := strings.Cut(spec, "=")
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("invalid source %q: want name=url", spec)
		}
		srcs = append(srcs, source{name: name, url: url})
	}
	return srcs, nil
}

// downloadTask is one planned GET.
type downloadTask struct {
	src source
	url string
	// offsetB is the starting byte of the Range request for fixed-size
	// sources — randomized so repeated cycles never fetch the identical
	// byte span of the same file (reads look like resumed transfers).
	offsetB int64
	wantB   int64
}

// planDownloads splits the cycle budget across sources: shuffle the list,
// then take round-robin passes, each pass drawing a random chunk (capped by
// remaining budget, remaining per-host cap, and the source's size) per
// source. Fixed-size sources also get a random Range offset into the file.
// Pure — no I/O — so it is directly unit-testable and deterministic for a
// given rng state.
func planDownloads(rng *rand.Rand, sources []source, budgetB, maxPerHostB int64) []downloadTask {
	if budgetB <= 0 || maxPerHostB <= 0 || len(sources) == 0 {
		return nil
	}
	order := slices.Clone(sources)
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	var tasks []downloadTask
	hostUsed := make(map[string]int64, len(order))
	remaining := budgetB
	for remaining > 0 {
		progressed := false
		for _, src := range order {
			if remaining <= 0 {
				break
			}
			perHostLeft := maxPerHostB - hostUsed[src.name]
			if perHostLeft <= 0 {
				continue
			}
			capB := min(remaining, perHostLeft)
			chunk := capB
			offset := int64(0)
			if src.approxB > 0 {
				capB = min(capB, src.approxB)
				if capB < minChunkB {
					continue
				}
				// Vary the drawn size (40%..100% of the cap) so the same
				// source does not serve the same amount every cycle.
				chunk = randInt64(rng, max(minChunkB, capB*2/5), capB)
				offset = randInt64(rng, 0, src.approxB-chunk)
			}
			if chunk < minChunkB {
				continue
			}
			url := src.url
			if strings.Contains(url, "%d") {
				url = fmt.Sprintf(url, chunk)
			}
			tasks = append(tasks, downloadTask{src: src, url: url, offsetB: offset, wantB: chunk})
			hostUsed[src.name] += chunk
			remaining -= chunk
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return tasks
}

// randInt64 draws a uniform random int64 in [lo, hi] with lo <= hi.
func randInt64(rng *rand.Rand, lo, hi int64) int64 {
	if hi <= lo {
		return lo
	}
	return lo + rng.Int64N(hi-lo+1)
}

// netStats aggregates the network phase outcome for the cycle summary.
type netStats struct {
	totalB   int64
	perHostB map[string]int64
	failures int
	skipped  int
}

// runDownloads executes planned tasks sequentially, rotating the destination
// file across dirs (both disks), pausing 1-3s between requests. Sequential +
// paused keeps per-host request rates trivially low.
func (k *Keeper) runDownloads(ctx context.Context, tasks []downloadTask, dirs []string) netStats {
	stats := netStats{perHostB: make(map[string]int64, len(tasks))}
	if len(tasks) == 0 || len(dirs) == 0 {
		return stats
	}
	client := &http.Client{} // per-request contexts bound duration
	for i, task := range tasks {
		if ctx.Err() != nil {
			stats.skipped = len(tasks) - i
			break
		}
		k.downloadOne(ctx, client, task, dirs[i%len(dirs)], &stats)
		if i == len(tasks)-1 || ctx.Err() != nil {
			break
		}
		// 0.5-5s randomized pause between requests keeps per-host request
		// rates low and avoids a metronomic fetch pattern.
		pause := time.Duration(500+k.rng.IntN(4500)) * time.Millisecond
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return stats
}

// downloadOne fetches one task into dir, syncs it to disk, removes the file,
// and records the byte count. Short reads (server closed early) still count
// what arrived; only hard failures increment failures.
func (k *Keeper) downloadOne(ctx context.Context, client *http.Client, task downloadTask, dir string, stats *netStats) {
	start := time.Now()
	reqCtx, cancel := context.WithTimeout(ctx, k.cfg.Net.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, task.url, http.NoBody)
	if err != nil {
		slog.Warn("download: build request", "source", task.src.name, "error", err)
		stats.failures++
		return
	}
	if task.src.approxB > 0 && task.wantB > 0 {
		// Partial content with a random start offset (see downloadTask.offsetB).
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", task.offsetB, task.offsetB+task.wantB-1))
	}
	resp, err := client.Do(req)
	if err != nil {
		logNetError(task, err)
		stats.failures++
		return
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			slog.Warn("download: close response body", "source", task.src.name, "error", cerr)
		}
	}()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		slog.Warn("download: unexpected status", "source", task.src.name, "status", resp.StatusCode)
		stats.failures++
		return
	}

	path := filepath.Join(dir, fmt.Sprintf("dl-%s-%d.bin", task.src.name, time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		slog.Warn("download: create temp file", "path", path, "error", err)
		stats.failures++
		return
	}
	// CopyN caps the read even when the server ignores Range (plain 200).
	n, copyErr := io.CopyN(f, resp.Body, task.wantB)
	syncErr := f.Sync() // force dirty pages to the platter before removal
	closeErr := f.Close()

	if n > 0 {
		stats.totalB += n
		stats.perHostB[task.src.name] += n
		d := time.Since(start)
		slog.Info("download ok", "source", task.src.name, "mb", mb(n), "seconds", d.Seconds(),
			"mbps", float64(n)/1e6/d.Seconds())
	}
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		logNetError(task, copyErr)
	}
	if syncErr != nil {
		slog.Warn("download: sync file", "path", path, "error", syncErr)
	}
	if closeErr != nil {
		slog.Warn("download: close file", "path", path, "error", closeErr)
	}
	// Retention mode keeps the file on disk (purged at the high-water mark);
	// delete mode removes it right away — the run-dir cleanup is the backstop.
	if !k.cfg.Disk.RetainFiles {
		if removeErr := os.Remove(path); removeErr != nil {
			slog.Warn("download: remove file", "path", path, "error", removeErr)
		}
	}
}

// logNetError suppresses the warn when the cycle context was cancelled
// mid-flight (shutdown) — that is not a source problem.
func logNetError(task downloadTask, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	slog.Warn("download failed", "source", task.src.name, "error", err)
}

// mb formats bytes as MiB for logs.
func mb(b int64) float64 {
	return float64(b) / (1 << 20)
}

// resolveSources returns the configured override list or the built-ins.
func resolveSources(cfg *config.NetConfig) ([]source, error) {
	if len(cfg.Sources) == 0 {
		return builtinSources(), nil
	}
	return parseSources(cfg.Sources)
}
