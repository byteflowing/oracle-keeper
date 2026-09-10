package keeper

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlanDownloads(t *testing.T) {
	sources := []source{
		{name: "param", url: "https://speed.example/__down?bytes=%d"},
		{name: "fixed-a", url: "https://a.example/file.bin", approxB: 10 << 20},
		{name: "fixed-b", url: "https://b.example/file.bin", approxB: 20 << 20},
	}

	t.Run("budget split across hosts within caps", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(1, 2))
		budget := int64(50 << 20)
		perHost := int64(20 << 20)
		tasks := planDownloads(rng, sources, budget, perHost)

		var total int64
		perHostSum := map[string]int64{}
		for _, task := range tasks {
			total += task.wantB
			perHostSum[task.src.name] += task.wantB
			require.GreaterOrEqual(t, task.wantB, int64(minChunkB))
			// Random Range offsets must stay inside the file.
			if task.src.approxB > 0 {
				require.GreaterOrEqual(t, task.offsetB, int64(0))
				require.LessOrEqual(t, task.offsetB+task.wantB, task.src.approxB)
			}
		}
		// Random chunk sizes leave at most a sub-minChunkB tail unspent.
		require.GreaterOrEqual(t, total, budget-minChunkB+1)
		require.LessOrEqual(t, total, budget)
		// Per-host cap respected on every source.
		for host, got := range perHostSum {
			require.LessOrEqual(t, got, perHost, "host %s over cap", host)
		}
		// Multiple hosts participated — traffic is spread.
		require.Len(t, perHostSum, 3)
	})

	t.Run("chunk sizes vary between draws", func(t *testing.T) {
		// Same fixed source, different rng states → different (chunk, offset)
		// pairs (probability of identical pairs is negligible).
		fixed := []source{{name: "fixed", url: "https://x.example/file.bin", approxB: 100 << 20}}
		seen := map[[2]int64]bool{}
		for seed := uint64(1); seed <= 20; seed++ {
			rng := rand.New(rand.NewPCG(seed, seed))
			budget := int64(10 << 20)
			tasks := planDownloads(rng, fixed, budget, 250<<20)
			require.NotEmpty(t, tasks)
			var total int64
			for _, task := range tasks {
				seen[[2]int64{task.offsetB, task.wantB}] = true
				total += task.wantB
			}
			// Random chunking still spends the budget down to a sub-MiB tail.
			require.GreaterOrEqual(t, total, budget-minChunkB+1)
			require.LessOrEqual(t, total, budget)
		}
		require.Greater(t, len(seen), 1, "planner should vary chunk size/offset")
	})

	t.Run("parametrized source carries exact bytes in url", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(3, 4))
		tasks := planDownloads(rng, []source{sources[0]}, 5<<20, 250<<20)
		require.NotEmpty(t, tasks)
		require.Equal(t, "https://speed.example/__down?bytes=5242880", tasks[0].url)
	})

	t.Run("fixed source capped at approx size", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(5, 6))
		tasks := planDownloads(rng, []source{sources[1]}, 100<<20, 250<<20)
		require.NotEmpty(t, tasks)
		for _, task := range tasks {
			require.LessOrEqual(t, task.wantB, int64(10<<20))
		}
	})

	t.Run("zero budget yields no tasks", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(7, 8))
		require.Empty(t, planDownloads(rng, sources, 0, 20<<20))
	})

	t.Run("no sources yields no tasks", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(9, 10))
		require.Empty(t, planDownloads(rng, nil, 50<<20, 20<<20))
	})

	t.Run("caps smaller than min chunk leave budget unspent", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(11, 12))
		tasks := planDownloads(rng, sources, 50<<20, 500<<10)
		require.Empty(t, tasks)
	})
}

func TestParseSources(t *testing.T) {
	t.Run("valid pairs", func(t *testing.T) {
		srcs, err := parseSources([]string{"a=https://a.example/f", "b=https://b.example/__down?bytes=%d"})
		require.NoError(t, err)
		require.Len(t, srcs, 2)
		require.Equal(t, "a", srcs[0].name)
		require.Equal(t, "https://a.example/f", srcs[0].url)
		require.Zero(t, srcs[1].approxB)
	})

	t.Run("missing separator", func(t *testing.T) {
		_, err := parseSources([]string{"no-separator"})
		require.ErrorContains(t, err, "want name=url")
	})

	t.Run("empty name", func(t *testing.T) {
		_, err := parseSources([]string{"=https://a.example/f"})
		require.ErrorContains(t, err, "want name=url")
	})
}

func TestJitterBudget(t *testing.T) {
	t.Run("within ±20%", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(13, 14))
		base := int64(700 << 20)
		for range 100 {
			got := jitterBudget(rng, base)
			require.InDelta(t, float64(base), float64(got), 0.21*float64(base))
		}
	})
	t.Run("zero base stays zero", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(15, 16))
		require.Zero(t, jitterBudget(rng, 0))
	})
}

func TestBuiltinSources(t *testing.T) {
	srcs := builtinSources()
	require.NotEmpty(t, srcs)
	names := make([]string, 0, len(srcs))
	for _, s := range srcs {
		names = append(names, s.name)
		require.NotEmpty(t, s.url)
		require.True(t, strings.HasPrefix(s.url, "https://"), "source %s not https", s.name)
		if s.approxB == 0 {
			require.Contains(t, s.url, "%d", "zero approx requires parametrized url: %s", s.name)
		}
	}
	joined := fmt.Sprint(names)
	require.Contains(t, joined, "cloudflare-speed")
	require.Contains(t, joined, "npmjs")
}
