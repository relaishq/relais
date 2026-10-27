// Package artifacts plans a verified release A→B→A without deployment effects.
package artifacts

import "context"

const (
	MaxArchiveBytes   = 256 << 20
	MaxChecksumsBytes = 64 << 10
	MaxManifestBytes  = 16 << 20
	MaxAssetPages     = 20
)

type Refusal struct {
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	Version string `json:"version,omitempty"`
	Compat  string `json:"compat,omitempty"`
}

func (r *Refusal) Error() string       { return r.Code + ": " + r.Detail }
func refuse(code, detail string) error { return &Refusal{Code: code, Detail: detail} }

type Release struct {
	Tag        string `json:"tag"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	TagCommit  string `json:"tagCommit"`
}
type Asset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	State  string `json:"state"`
	Digest string `json:"digest,omitempty"`
}
type ReleaseReply struct {
	State   string
	Tag     string
	Release Release
}
type AssetPage struct {
	Tag, Token, NextToken string
	Assets                []Asset
	Complete              bool
}
type Download struct {
	Tag     string
	AssetID int64
	Bytes   []byte
}
type File struct {
	State, Commit, Path string
	Bytes               []byte
}

// Reader is a read-only port. Errors never mean not-found. Replies echo the
// requested identity so a delayed response cannot qualify another request.
type Reader interface {
	Release(context.Context, string) (ReleaseReply, error)
	Assets(context.Context, string, string) (AssetPage, error)
	Download(context.Context, string, Asset) (Download, error)
	File(context.Context, string, string) (File, error)
}
type PinRequest struct {
	Version       string `json:"version"`
	ArchiveSHA256 string `json:"archiveSha256"`
}
type Request struct {
	From PinRequest `json:"from"`
	To   PinRequest `json:"to"`
	Arch string     `json:"arch"`
}
type Pin struct {
	Asset           Asset  `json:"asset"`
	Version         string `json:"version"`
	Tag             string `json:"tag"`
	Commit          string `json:"commit"`
	Arch            string `json:"arch"`
	Archive         string `json:"archive"`
	ArchiveSHA256   string `json:"archiveSha256"`
	ChecksumsSHA256 string `json:"checksumsSha256"`
	ManifestSHA256  string `json:"manifestSha256"`
}
type Parameters struct {
	ReleaseVersion string
	Architecture   string
	ArchiveSHA256  string
}
type Step struct {
	Operation  string     `json:"operation"`
	Parameters Parameters `json:"parameters"`
}
type Plan struct {
	Version int    `json:"version"`
	Arch    string `json:"arch"`
	From    Pin    `json:"from"`
	To      Pin    `json:"to"`
	Steps   []Step `json:"steps"`
}
