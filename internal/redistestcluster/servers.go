package redistestcluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/processidentity"
)

const serverHarness = "relais-redis-cluster-v1"

type serverRecord struct {
	Harness        string
	Parent, Server processidentity.Identity
	PIDFile        string
}

func registryRoot() string { return filepath.Join(artifactRoot(), "server-registry") }

// Only records made immediately after our own StartSupervised are admitted.
// No process list, port search or PID-only kill is used to discover candidates.
func recordServer(child *clusterprocess.Child, pidFile string) (string, error) {
	parent, err := processidentity.Current()
	if err != nil {
		return "", err
	}
	server, err := processidentity.Observe(child.PID())
	if err != nil {
		return "", err
	}
	root := registryRoot()
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if err := privateRegistry(root); err != nil {
		return "", err
	}
	record := serverRecord{Harness: serverHarness, Parent: parent, Server: server, PIDFile: pidFile}
	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(root, "server-*.pending")
	if err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	path := strings.TrimSuffix(file.Name(), ".pending") + ".json"
	return path, os.Rename(file.Name(), path)
}

func cleanupStaleServers(ctx context.Context) error {
	root := registryRoot()
	if err := privateRegistry(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "server-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("server ownership record cannot be a symbolic link")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("server ownership records must be private regular files")
		}
		if err := registryOwner(info); err != nil {
			return err
		}
		path := filepath.Join(root, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record serverRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("invalid owned server record: %w", err)
		}
		if record.Harness != serverHarness {
			return errors.New("unrecognized server ownership record")
		}
		parentGone, err := processidentity.Gone(ctx, record.Parent)
		if err != nil {
			return err
		}
		if !parentGone {
			continue
		}
		serverGone, err := processidentity.Gone(ctx, record.Server)
		if err != nil {
			return err
		}
		if !serverGone {
			pid, err := os.ReadFile(record.PIDFile)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("cannot verify owned Redis pid file: %w", err)
			}
			number, err := strconv.Atoi(strings.TrimSpace(string(pid)))
			if len(pid) > 0 && (err != nil || number != record.Server.PID) {
				return errors.New("owned Redis pid file does not match its recorded identity")
			}
			fenceCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = processidentity.Fence(fenceCtx, record.Server)
			cancel()
			if err != nil {
				return err
			}
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func privateRegistry(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("server registry must be a private directory")
	}
	return registryOwner(info)
}
