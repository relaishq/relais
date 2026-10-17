package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/relaishq/relais/internal/nettopology"
)

func reclaimRecorded(ctx context.Context, root string) error {
	var owners []nettopology.OwnerRecord
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() != "namespace-owner.json" || !entry.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var owner nettopology.OwnerRecord
		if err = json.Unmarshal(b, &owner); err != nil {
			return err
		}
		if owner.UID != os.Getuid() || owner.PID <= 0 {
			return fmt.Errorf("invalid cleanup owner record: %s", path)
		}
		owners = append(owners, owner)
		return nil
	})
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(owners) == 0 {
		log.Print("no recorded namespace owners to reclaim")
		return nil
	}
	return nettopology.ReclaimStaleOwners(ctx, owners, log.Printf)
}
