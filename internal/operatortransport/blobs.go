package operatortransport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"regexp"

	"github.com/relaishq/relais/pkg/operator"
)

var blobName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validBlob(ref operator.Blob, limit int64) bool {
	return blobName.MatchString(ref.Name) && hashPattern.MatchString(ref.SHA256) && ref.Size >= 0 && limit >= 0 && ref.Size <= min(limit, int64(operator.MaxBlobBytes))
}
func validReplyBlob(r operator.Reply) bool {
	return r.Blob == nil || (len(r.Payload) == 0 && validBlob(*r.Blob, operator.MaxBlobBytes))
}

// ResolveReply owns blob validation before any operation classifier sees bytes.
// A missing agreed spool is a refusal, never permission to trust inline evidence.
func ResolveReply(root *os.Root, r operator.Reply) (operator.Reply, error) {
	if !validReplyBlob(r) {
		return operator.Reply{}, errors.New("invalid-blob")
	}
	if r.Blob == nil {
		return r, nil
	}
	if root == nil {
		return operator.Reply{}, errors.New("invalid-blob")
	}
	b, err := ReadBlob(root, *r.Blob, operator.MaxBlobBytes)
	if err != nil {
		return operator.Reply{}, err
	}
	r.Payload = b
	r.Blob = nil
	return r, nil
}

// ReadBlob never emits the local spool path. os.Root confines even symlink
// resolution; flat opaque names cannot select arbitrary adapter files.
func ReadBlob(root *os.Root, ref operator.Blob, limit int64) ([]byte, error) {
	invalid := errors.New("invalid-blob")
	if !validBlob(ref, limit) {
		return nil, invalid
	}
	f, err := root.Open(ref.Name)
	if err != nil {
		return nil, invalid
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != ref.Size {
		return nil, invalid
	}
	b, err := io.ReadAll(io.LimitReader(f, ref.Size+1))
	sum := sha256.Sum256(b)
	if err != nil || int64(len(b)) != ref.Size || hex.EncodeToString(sum[:]) != ref.SHA256 {
		return nil, invalid
	}
	return b, nil
}
