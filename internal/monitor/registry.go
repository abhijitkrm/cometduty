package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Public-endpoint fallback: resolve chain_id → registry path → first RPC
// address listed in the chain registry's apis.rpc section. This is opt-in per
// chain (public_fallback) and only used when every configured node is dead.

const registryIndexURL = "https://chains.cosmos.directory/"
const registryChainURL = "https://chains.cosmos.directory/%s"

var (
	regMu    sync.Mutex
	regPaths map[string]string // chain_id -> registry path
	regAt    time.Time
)

// refreshRegistryIndex (re)loads the chain_id → path map at most hourly.
func refreshRegistryIndex(ctx context.Context) error {
	regMu.Lock()
	defer regMu.Unlock()
	if regPaths != nil && time.Since(regAt) < time.Hour {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryIndexURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var idx struct {
		Chains []struct {
			Path    string `json:"path"`
			ChainID string `json:"chain_id"`
		} `json:"chains"`
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return err
	}
	m := map[string]string{}
	for _, c := range idx.Chains {
		m[c.ChainID] = c.Path
	}
	regPaths, regAt = m, time.Now()
	return nil
}

// publicEndpoints returns candidate public RPC URLs for chainID from the chain
// registry.
func publicEndpoints(ctx context.Context, chainID string) ([]string, error) {
	if err := refreshRegistryIndex(ctx); err != nil {
		return nil, err
	}
	regMu.Lock()
	path := regPaths[chainID]
	regMu.Unlock()
	if path == "" {
		return nil, fmt.Errorf("no registry entry for %s", chainID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(registryChainURL, path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var ch struct {
		Apis struct {
			RPC []struct {
				Address string `json:"address"`
			} `json:"rpc"`
		} `json:"apis"`
	}
	if err := json.Unmarshal(b, &ch); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ch.Apis.RPC))
	for _, r := range ch.Apis.RPC {
		if r.Address != "" {
			out = append(out, r.Address)
		}
	}
	return out, nil
}
