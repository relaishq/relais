package redisendpoint

import (
	"strings"
	"testing"
)

func TestAddressPolicy(t *testing.T) {
	for _, optIn := range []string{"", "0", "1"} {
		t.Run("opt_in_"+optIn, func(t *testing.T) {
			t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", optIn)
			for _, host := range []string{"localhost", "LOCALHOST", "localhost.", "127.0.0.1", "127.8.9.10", "[::1]", "[::ffff:127.0.0.2]"} {
				for _, port := range []string{"6379", "06379", "0006379"} {
					addr := host + ":" + port
					err := Validate(addr)
					if (err == nil) != (optIn == "1") {
						t.Errorf("Validate(%q): %v", addr, err)
					}
					if err != nil && (!strings.Contains(err.Error(), "RELAIS_REDIS_ALLOW_LOCAL_6379=1") || !strings.Contains(err.Error(), "protect")) {
						t.Errorf("missing rationale/opt-in: %v", err)
					}
					if ValidateDev(addr) == nil {
						t.Errorf("dev accepted %q with opt-in %q", addr, optIn)
					}
				}
				if err := Validate(host + ":16379"); err != nil {
					t.Error(err)
				}
				if err := ValidateDev(host + ":16379"); err != nil {
					t.Error(err)
				}
			}
			// No DNS lookup: even an unresolvable hostname is classified as remote.
			for _, host := range []string{"cache.example.invalid", "10.20.30.40", "[2001:db8::2]", "[::ffff:10.0.0.1]"} {
				if err := Validate(host + ":6379"); err != nil {
					t.Error(err)
				}
				if ValidateDev(host+":6379") == nil {
					t.Errorf("dev accepted remote 6379: %q", host)
				}
				if err := ValidateDev(host + ":16379"); err != nil {
					t.Error(err)
				}
			}
		})
	}
}

func TestInvalidAddressAndOptIn(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "1")
	for _, addr := range []string{"", "missing-port", ":6379", ":16379", "0.0.0.0:6379", "[::]:6379", "[::ffff:0.0.0.0]:6379", "0.0.0.0:16379", "[::]:16379", "127.0.0.1:redis", "127.0.0.1:0", "127.0.0.1:65536", "user:password@cache:6379"} {
		if Validate(addr) == nil {
			t.Errorf("accepted %q", addr)
		}
		if ValidateDev(addr) == nil {
			t.Errorf("dev accepted %q", addr)
		}
	}
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "secret-invalid-value")
	err := Validate("cache.example.invalid:6379")
	if err == nil || strings.Contains(err.Error(), "secret-invalid-value") {
		t.Fatalf("bad switch error: %v", err)
	}
	if err := ValidateDev("127.0.0.1:16379"); err != nil {
		t.Fatalf("dev must ignore opt-in entirely: %v", err)
	}
}
