package redisendpoint

import "testing"

func TestValidateBeforeDial(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:6379", "127.0.0.1:06379", "[::1]:0006379", "127.0.0.1:redis", "127.0.0.1:0", "127.0.0.1:65536"} {
		if Validate(addr) == nil {
			t.Fatalf("accepted reserved or invalid endpoint %q", addr)
		}
	}
	if err := Validate("127.0.0.1:16379"); err != nil {
		t.Fatal(err)
	}
}
