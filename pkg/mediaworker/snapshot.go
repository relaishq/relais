package mediaworker

// SnapshotSession copies a live session without fencing or dropping it. The
// result has the same resumable format as ExportSession. Only the copy takes
// the packet lock; binary/JSON encoding happens after releasing it.
func (w *Worker) SnapshotSession(id string) ([]byte, error) {
	s := w.session(id)
	if s == nil {
		return nil, ErrUnknownSession
	}
	return s.checkpoint.capture(checkpointLive, nil)
}
