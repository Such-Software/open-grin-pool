package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config the loader should accept: logical names only, every secret left empty.
const secretlessConfig = `{
  "log": {"level": "info", "file": ""},
  "stratum_server": {"address": "0.0.0.0", "port": 3344, "backup_interval": "6h"},
  "api_server": {"address": "127.0.0.1", "port": 3333, "auth_user": "lepool", "auth_pass": ""},
  "storage": {"address": "127.0.0.1", "port": 6379, "db": 0, "password": ""},
  "node": {"address": "127.0.0.1", "api_port": 3413, "stratum_port": 3416,
           "auth_user": "grin", "auth_pass": "", "diff": 50000, "block_time": 60},
  "wallet": {"address": "127.0.0.1", "owner_api_version": "v3", "owner_api_port": 3420,
             "auth_user": "grin", "auth_pass": ""},
  "payer": {"time": "23:59", "fee": 0.005}
}`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func setSecrets(t *testing.T) {
	t.Helper()
	for _, n := range []string{envNodeAuthPass, envWalletAuthPass, envAPIAuthPass} {
		t.Setenv(n, "value-for-this-test-only")
	}
}

func TestSecretsComeFromTheEnvironmentAndNotTheFile(t *testing.T) {
	setSecrets(t)
	conf, err := loadConfig(writeConfig(t, secretlessConfig))
	if err != nil {
		t.Fatalf("a secretless config was rejected: %v", err)
	}
	for where, got := range map[string]string{
		"node":   conf.Node.AuthPass,
		"wallet": conf.Wallet.AuthPass,
		"api":    conf.APIServer.AuthPass,
	} {
		if got == "" {
			t.Errorf("%s auth pass was not filled from the environment", where)
		}
	}
}

func TestALiteralSecretInTheFileIsRefusedAndSaysWhere(t *testing.T) {
	// Upstream shipped a config.json with literal auth_pass values. Honouring one means
	// the value is in Git and nobody finds out until a rotation, so refuse it instead.
	setSecrets(t)
	for _, field := range []string{"node", "wallet", "api_server", "storage"} {
		t.Run(field, func(t *testing.T) {
			key := "auth_pass"
			if field == "storage" {
				key = "password"
			}
			body := strings.Replace(secretlessConfig,
				`"`+key+`": ""`, `"`+key+`": "a-literal-value"`, 1)
			if field == "wallet" || field == "api_server" || field == "storage" {
				// replace the Nth occurrence rather than the first
				body = secretlessConfig
				idx := strings.Index(body, `"`+field+`"`)
				if idx < 0 {
					t.Fatalf("fixture has no %s section", field)
				}
				rest := strings.Replace(body[idx:], `"`+key+`": ""`,
					`"`+key+`": "a-literal-value"`, 1)
				body = body[:idx] + rest
			}
			_, err := loadConfig(writeConfig(t, body))
			if err == nil {
				t.Fatalf("a literal secret in %s.%s was accepted", field, key)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("refusal does not name the offending field, got: %v", err)
			}
		})
	}
}

func TestAMissingRequiredSecretRefusesAndNamesTheVariable(t *testing.T) {
	// A pool that starts with an empty RPC password authenticates against nothing and
	// fails later as a confusing 401 from the node. Fail here, naming the variable.
	setSecrets(t)
	t.Setenv(envWalletAuthPass, "")
	_, err := loadConfig(writeConfig(t, secretlessConfig))
	if err == nil {
		t.Fatal("a missing wallet secret was accepted")
	}
	if !strings.Contains(err.Error(), envWalletAuthPass) {
		t.Errorf("refusal does not name the missing variable, got: %v", err)
	}
}

func TestTheStorageSecretIsOptionalBecauseRedisMayHaveNoPassword(t *testing.T) {
	setSecrets(t)
	os.Unsetenv(envStoragePass)
	if _, err := loadConfig(writeConfig(t, secretlessConfig)); err != nil {
		t.Fatalf("an unset optional storage secret was refused: %v", err)
	}
}

func TestTheConfigPathIsOverridable(t *testing.T) {
	// The unit points at /etc/lepool/config.json rather than running from that directory.
	t.Setenv("LEPOOL_CONFIG", "/etc/lepool/config.json")
	if got := configPath(); got != "/etc/lepool/config.json" {
		t.Errorf("LEPOOL_CONFIG ignored, got %q", got)
	}
	t.Setenv("LEPOOL_CONFIG", "")
	if got := configPath(); got != "config.json" {
		t.Errorf("default config path changed, got %q", got)
	}
}

func TestTheShippedConfigCarriesNoSecrets(t *testing.T) {
	// The repository's own config.json is the one most likely to grow a literal by
	// accident, because it is the one people copy.
	setSecrets(t)
	if _, err := loadConfig("config.json"); err != nil {
		t.Fatalf("the repository's config.json is not loadable secretless: %v", err)
	}
}
