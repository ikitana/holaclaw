package check

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNET001(t *testing.T) {
	cases := []struct {
		name     string
		listen   string
		cert     string
		key      string
		insecure bool
		want     int
	}{
		{"unset", "", "", "", false, 0},
		{"loopback no tls", "127.0.0.1:8080", "", "", false, 0},
		{"localhost no tls", "localhost:8080", "", "", false, 0},
		{"non-loopback no tls", "0.0.0.0:8080", "", "", false, 1},
		{"non-loopback with tls", "0.0.0.0:8080", "cert.pem", "key.pem", false, 0},
		{"non-loopback missing key", "0.0.0.0:8080", "cert.pem", "", false, 1},
		{"non-loopback plain http acknowledged", "0.0.0.0:8080", "", "", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.Server.Listen, cfg.Server.TLSCert, cfg.Server.TLSKey = c.listen, c.cert, c.key
			cfg.Server.AllowInsecureTransport = c.insecure
			fs := net001Rule().Run(context.Background(), &Input{Config: cfg})
			if len(fs) != c.want {
				t.Fatalf("findings = %+v, want %d", fs, c.want)
			}
			if c.want == 1 && fs[0].ID != "NET001" {
				t.Errorf("id = %s", fs[0].ID)
			}
		})
	}
}

func TestNET002(t *testing.T) {
	t.Run("no tls", func(t *testing.T) {
		fs := net002Rule().Run(context.Background(), &Input{Config: minimalConfig()})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("files present", func(t *testing.T) {
		dir := t.TempDir()
		cert := filepath.Join(dir, "cert.pem")
		key := filepath.Join(dir, "key.pem")
		if err := os.WriteFile(cert, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := minimalConfig()
		cfg.Server.TLSCert, cfg.Server.TLSKey = cert, key
		fs := net002Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("missing files", func(t *testing.T) {
		dir := t.TempDir()
		cfg := minimalConfig()
		cfg.Server.TLSCert = filepath.Join(dir, "missing-cert.pem")
		cfg.Server.TLSKey = filepath.Join(dir, "missing-key.pem")
		fs := net002Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 2 {
			t.Fatalf("findings = %+v, want 2", fs)
		}
	})
}

func TestNET003(t *testing.T) {
	cases := []struct {
		name, auth, base, modelURL string
		want                       int
	}{
		{"no auth over http", "none", "http://10.0.0.5:11434/v1", "", 0},
		{"key over https", "api-key", "https://openrouter.ai/api/v1", "https://openrouter.ai/api/alpha/decisions", 0},
		{"key over loopback http", "api-key", "http://127.0.0.1:8000/v1", "", 0},
		{"key over plain http", "api-key", "http://api.example.com/v1", "", 1},
		{"model url over plain http", "api-key", "https://openrouter.ai/api/v1", "http://api.example.com/decisions", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := minimalConfig()
			p := cfg.Providers["ollama"]
			p.Auth, p.BaseURL = c.auth, c.base
			cfg.Providers["ollama"] = p
			m := cfg.Models["gemma"]
			m.URL = c.modelURL
			cfg.Models["gemma"] = m
			fs := net003Rule().Run(context.Background(), &Input{Config: cfg})
			if len(fs) != c.want {
				t.Fatalf("findings = %+v, want %d", fs, c.want)
			}
		})
	}
}
