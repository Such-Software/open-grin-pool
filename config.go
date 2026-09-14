package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type config struct {
	Log struct {
		Level string `json:"level"`
		File  string `json:"file"`
	} `json:"log"`
	StratumServer struct {
		Address         string   `json:"address"`
		Port            int      `json:"port"`
		BackupInterval  string   `json:"backup_interval"`
		OmitAgentStatus []string `json:"omit_agent_status"`
	} `json:"stratum_server"`
	APIServer struct {
		Address  string `json:"address"`
		Port     int    `json:"port"`
		AuthUser string `json:"auth_user"`
		AuthPass string `json:"auth_pass"`
	} `json:"api_server"`
	Storage struct {
		Address  string `json:"address"`
		Port     int    `json:"port"`
		Db       int    `json:"db"`
		Password string `json:"password"`
	} `json:"storage"`
	Node struct {
		Address     string `json:"address"`
		APIPort     int    `json:"api_port"`
		StratumPort int    `json:"stratum_port"`
		AuthUser    string `json:"auth_user"`
		AuthPass    string `json:"auth_pass"`
		Diff        int    `json:"diff"`
		BlockTime   int    `json:"block_time"`
	} `json:"node"`
	Wallet struct {
		Address         string `json:"address"`
		OwnerAPIVersion string `json:"owner_api_version"`
		OwnerAPIPort    int    `json:"owner_api_port"`
		AuthUser        string `json:"auth_user"`
		AuthPass        string `json:"auth_pass"`
	} `json:"wallet"`
	Payer struct {
		Time string  `json:"time"`
		Fee  float64 `json:"fee"`
	} `json:"payer"`
}

// Credential values never live in the repository. Upstream shipped a config.json with
// literal auth_pass strings, which have been public on GitHub since 2020; ours are read
// from the environment at start-up and the config file carries only logical names.
//
// Each of these names a secret the operator exports before the unit starts:
const (
	envNodeAuthPass   = "LEPOOL_NODE_AUTH_PASS"
	envWalletAuthPass = "LEPOOL_WALLET_AUTH_PASS"
	envAPIAuthPass    = "LEPOOL_API_AUTH_PASS"
	envStoragePass    = "LEPOOL_STORAGE_PASS"
)

// configPath is the file to read; LEPOOL_CONFIG overrides it so a unit can point at
// /etc/lepool/config.json without the daemon having to run from that directory.
func configPath() string {
	if p := os.Getenv("LEPOOL_CONFIG"); p != "" {
		return p
	}
	return "config.json"
}

// secretFromEnv refuses rather than falling back. A pool that silently starts with an
// empty RPC password authenticates against nothing and the failure surfaces later as a
// confusing 401 from the node, so say which variable is missing and stop here.
func secretFromEnv(name string, required bool) (string, error) {
	v := os.Getenv(name)
	if v == "" && required {
		return "", fmt.Errorf("%s is not set; export it before starting lepool", name)
	}
	return v, nil
}

func parseConfig() *config {
	conf, err := loadConfig(configPath())
	if err != nil {
		panic(err)
	}
	return conf
}

func loadConfig(path string) (*config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var conf config
	if err := json.NewDecoder(f).Decode(&conf); err != nil {
		return nil, err
	}

	// A literal secret in the file is a mistake we refuse rather than honour, because
	// honouring it means the value is now in Git and nobody finds out until a rotation.
	for _, got := range []struct{ where, value string }{
		{"node.auth_pass", conf.Node.AuthPass},
		{"wallet.auth_pass", conf.Wallet.AuthPass},
		{"api_server.auth_pass", conf.APIServer.AuthPass},
		{"storage.password", conf.Storage.Password},
	} {
		if strings.TrimSpace(got.value) != "" {
			return nil, fmt.Errorf(
				"%s carries a literal value in %s; credentials come from the environment, "+
					"leave it empty and export the matching LEPOOL_* variable", got.where, path)
		}
	}

	for _, bind := range []struct {
		env      string
		into     *string
		required bool
	}{
		{envNodeAuthPass, &conf.Node.AuthPass, true},
		{envWalletAuthPass, &conf.Wallet.AuthPass, true},
		{envAPIAuthPass, &conf.APIServer.AuthPass, true},
		{envStoragePass, &conf.Storage.Password, false},
	} {
		v, err := secretFromEnv(bind.env, bind.required)
		if err != nil {
			return nil, err
		}
		*bind.into = v
	}

	return &conf, nil
}
