package sessionstore

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
)

// RedisKeyOptionsFromEnv reads the environment-only session-store key ring.
// OLD_KEYS is a JSON object mapping decimal byte IDs to hex/base64 AES-256
// keys. No error includes the key material. Key ID zero remains the default.
func RedisKeyOptionsFromEnv() (RedisOptions, error) { return RedisKeyOptions(os.Getenv) }

// RedisKeyOptions applies the same environment-only grammar to a child process environment.
func RedisKeyOptions(lookup func(string) string) (RedisOptions, error) {
	opts := RedisOptions{}
	if value := lookup("RELAIS_SESSIONSTORE_KEY_ID"); value != "" {
		id, err := strconv.ParseUint(value, 10, 8)
		if err != nil {
			return opts, errors.New("RELAIS_SESSIONSTORE_KEY_ID must be a decimal byte")
		}
		opts.KeyID = byte(id)
	}
	if value := lookup("RELAIS_SESSIONSTORE_OLD_KEYS"); value != "" {
		var encoded map[string]string
		if json.Unmarshal([]byte(value), &encoded) != nil || encoded == nil {
			return opts, errors.New("RELAIS_SESSIONSTORE_OLD_KEYS must be an object of key IDs and encoded keys")
		}
		opts.OldKeys = map[byte][]byte{}
		for text, value := range encoded {
			id, err := strconv.ParseUint(text, 10, 8)
			if err != nil || strconv.FormatUint(id, 10) != text || byte(id) == opts.KeyID {
				return opts, errors.New("RELAIS_SESSIONSTORE_OLD_KEYS needs unique canonical IDs distinct from the active key")
			}
			key, err := hex.DecodeString(value)
			if err != nil || len(key) != 32 {
				key, err = base64.StdEncoding.DecodeString(value)
			}
			if err != nil || len(key) != 32 {
				return opts, errors.New("RELAIS_SESSIONSTORE_OLD_KEYS entries must encode 32 bytes as hex or base64")
			}
			opts.OldKeys[byte(id)] = key
		}
	}
	return opts, nil
}
