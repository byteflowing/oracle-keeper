package keeper

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/servekit/oracle-keeper/pkg/config"
)

func TestDecideBusy(t *testing.T) {
	cfg := &config.BusyConfig{CPUPercent: 40, LoadFactor: 0.8}

	tests := []struct {
		name    string
		sample  LoadSample
		cores   int
		wantHit bool
	}{
		{name: "idle host", sample: LoadSample{CPUPercent: 5, Load1: 0.1}, cores: 4, wantHit: false},
		{name: "cpu at threshold", sample: LoadSample{CPUPercent: 40, Load1: 0.1}, cores: 4, wantHit: true},
		{name: "cpu above threshold", sample: LoadSample{CPUPercent: 55.5, Load1: 0.1}, cores: 4, wantHit: true},
		{name: "load at threshold", sample: LoadSample{CPUPercent: 5, Load1: 3.2}, cores: 4, wantHit: true},
		{name: "load just below threshold", sample: LoadSample{CPUPercent: 5, Load1: 3.19}, cores: 4, wantHit: false},
		{name: "single core box idle", sample: LoadSample{CPUPercent: 5, Load1: 0.7}, cores: 1, wantHit: false},
		{name: "single core box busy by load", sample: LoadSample{CPUPercent: 5, Load1: 0.9}, cores: 1, wantHit: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			busy, reason := decideBusy(tt.sample, cfg, tt.cores)
			require.Equal(t, tt.wantHit, busy)
			if tt.wantHit {
				require.NotEmpty(t, reason)
			} else {
				require.Empty(t, reason)
			}
		})
	}
}

func TestBurnCores(t *testing.T) {
	require.Equal(t, 3, burnCores(3))
	require.Positive(t, burnCores(0)) // 0 = all visible cores
}
