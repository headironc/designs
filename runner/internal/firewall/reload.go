// Package firewall preserves the dedicated service's nftables reload contract.
package firewall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Reload checks and applies one batch. It never retries an uncertain application.
// nft and rulesPath come from the entrypoint, not from guest or workflow input.
func Reload(ctx context.Context, nft, rulesPath string) error {
	listed, err := command(ctx, nft, nil, "-j", "list", "tables")
	if err != nil {
		return fmt.Errorf("list firewall tables: %w", err)
	}
	deletes, err := ownedDeletes(listed)
	if err != nil {
		return fmt.Errorf("read firewall table list: %w", err)
	}
	rules, err := os.ReadFile(rulesPath)
	if err != nil {
		return fmt.Errorf("read firewall rules: %w", err)
	}
	batch := append(deletes, rules...)
	if _, err := command(ctx, nft, batch, "--check", "-f", "-"); err != nil {
		return fmt.Errorf("check firewall rules: %w", err)
	}
	// Reuse the checked bytes even if the source file changes during the check.
	// nft commits this single batch atomically; a lost process result is not a retry.
	if _, err := command(ctx, nft, batch, "-f", "-"); err != nil {
		return fmt.Errorf("apply firewall rules (outcome may be unknown; inspect before retry): %w", err)
	}
	return nil
}

func command(ctx context.Context, nft string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, nft, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		return nil, fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return output, nil
}

func ownedDeletes(raw []byte) ([]byte, error) {
	var listing struct {
		Nftables []json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return nil, err
	}
	if listing.Nftables == nil {
		return nil, errors.New("missing nftables array")
	}
	owned := make(map[string]bool)
	for _, entry := range listing.Nftables {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(entry, &row); err != nil || row == nil {
			return nil, errors.New("invalid nftables entry")
		}
		table, ok := row["table"]
		if !ok {
			continue // nft also includes a metainfo entry.
		}
		var identity struct {
			Family string `json:"family"`
			Name   string `json:"name"`
		}
		if err := json.Unmarshal(table, &identity); err != nil || identity.Family == "" || identity.Name == "" {
			return nil, errors.New("invalid table identity")
		}
		if identity.Name == "nucleiron" {
			owned[identity.Family] = true
		}
	}
	var deletes []byte
	for _, family := range []string{"inet", "bridge"} {
		if owned[family] {
			deletes = append(deletes, "delete table "+family+" nucleiron\n"...)
		}
	}
	return deletes, nil
}
