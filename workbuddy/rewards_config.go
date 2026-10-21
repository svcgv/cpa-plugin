package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// rewardSettings is the resolved switch set for one account. Auto controls
// scheduled runs only; manual runs always use the actions the user selected.
type rewardSettings struct {
	Auto    bool
	Actions map[rewardAction]bool
	// GrowthAuto is set only by the legacy growth_auto key. A false value turns
	// off all growth actions while leaving the check-in action independent.
	GrowthAuto *bool
}

type rewardOverride struct {
	Auto    *bool
	Actions map[rewardAction]*bool
}

type rewardConfig struct {
	Defaults rewardSettings
	Accounts map[string]rewardOverride
	// LegacyGrowthAuto records whether growth_auto was present. The effective
	// switches are resolved by resolveRewardSettings.
	LegacyGrowthAuto *bool
}

var rewardConfigState = struct {
	sync.RWMutex
	cfg rewardConfig
}{
	cfg: defaultRewardConfig(),
}

func defaultRewardSettings() rewardSettings {
	actions := make(map[rewardAction]bool, len(rewardActionOrder))
	for _, action := range rewardActionOrder {
		actions[action] = true
	}
	return rewardSettings{Auto: true, Actions: actions}
}

func defaultRewardConfig() rewardConfig {
	return rewardConfig{Defaults: defaultRewardSettings(), Accounts: map[string]rewardOverride{}}
}

func cloneRewardSettings(in rewardSettings) rewardSettings {
	out := rewardSettings{Auto: in.Auto, Actions: make(map[rewardAction]bool, len(in.Actions)), GrowthAuto: in.GrowthAuto}
	for action, enabled := range in.Actions {
		out.Actions[action] = enabled
	}
	return out
}

func rewardSettingsSnapshot() rewardSettings {
	rewardConfigState.RLock()
	defer rewardConfigState.RUnlock()
	return cloneRewardSettings(rewardConfigState.cfg.Defaults)
}

// rewardAccountKey is stable across auth-file renames. It includes the plugin,
// service region, UID and enterprise; an account without a UID falls back to
// the host auth ID. Only the hash is persisted so unusual UID characters cannot
// break YAML keys or leak identifiers into logs.
func rewardAccountKey(provider, region, uid, enterpriseID, authID string) string {
	region = strings.TrimSpace(strings.ToLower(region))
	uid = strings.TrimSpace(uid)
	enterpriseID = strings.TrimSpace(enterpriseID)
	authID = strings.TrimSpace(authID)
	if region == "" {
		region = "cn"
	}
	if uid == "" {
		uid = "auth:" + authID
	}
	identity := strings.Join([]string{strings.TrimSpace(provider), region, uid, enterpriseID}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return region + "-" + hex.EncodeToString(sum[:])[:16]
}

func workbuddyRewardKey(sa *storedAuth, authID string) string {
	region := "cn"
	uid, enterpriseID := "", ""
	if sa != nil {
		region = accountRegion(sa)
		uid = sa.Account.UID
		enterpriseID = sa.Account.EnterpriseID
	}
	return rewardAccountKey("workbuddy", region, uid, enterpriseID, authID)
}

func resolveRewardSettings(cfg rewardConfig, key string) rewardSettings {
	out := cloneRewardSettings(cfg.Defaults)
	override, ok := cfg.Accounts[key]
	if !ok {
		return out
	}
	if override.Auto != nil {
		out.Auto = *override.Auto
		// An explicit account switch supersedes the legacy growth switch.
		out.GrowthAuto = nil
	}
	for action, enabled := range override.Actions {
		if enabled != nil {
			out.Actions[action] = *enabled
		}
	}
	return out
}

func rewardSettingsFor(sa *storedAuth, authID string) rewardSettings {
	rewardConfigState.RLock()
	defer rewardConfigState.RUnlock()
	return resolveRewardSettings(rewardConfigState.cfg, workbuddyRewardKey(sa, authID))
}

func setRewardConfig(cfg rewardConfig) {
	rewardConfigState.Lock()
	rewardConfigState.cfg = cfg
	rewardConfigState.Unlock()
}

func rewardConfigJSON() map[string]any {
	rewardConfigState.RLock()
	defer rewardConfigState.RUnlock()
	cfg := rewardConfigState.cfg
	actions := map[string]bool{}
	for _, action := range rewardActionOrder {
		actions[string(action)] = cfg.Defaults.Actions[action]
	}
	accounts := map[string]any{}
	for key, override := range cfg.Accounts {
		row := map[string]any{}
		if override.Auto != nil {
			row["auto"] = *override.Auto
		}
		if len(override.Actions) > 0 {
			act := map[string]bool{}
			for action, enabled := range override.Actions {
				if enabled != nil {
					act[string(action)] = *enabled
				}
			}
			row["actions"] = act
		}
		accounts[key] = row
	}
	out := map[string]any{"auto": cfg.Defaults.Auto, "actions": actions, "accounts": accounts}
	if cfg.Defaults.GrowthAuto != nil {
		out["growth_auto"] = *cfg.Defaults.GrowthAuto
	}
	return out
}

// parseRewardConfig validates rewards.<...> in plugin config. Unknown keys are
// rejected so a typo cannot silently leave a reward enabled.
func parseRewardConfig(root *yaml.Node) (rewardConfig, error) {
	cfg := defaultRewardConfig()
	if root == nil {
		return cfg, nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "growth_auto" {
			continue
		}
		v, err := yamlBool(root.Content[i+1], "growth_auto")
		if err != nil {
			return cfg, err
		}
		cfg.LegacyGrowthAuto = &v
		cfg.Defaults.GrowthAuto = &v
	}
	// checkin_auto is the pre-existing switch. It controls only the daily
	// check-in action unless rewards.actions.checkin is explicitly present.
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "checkin_auto" {
			continue
		}
		v, err := yamlBool(root.Content[i+1], "checkin_auto")
		if err != nil {
			return cfg, err
		}
		cfg.Defaults.Actions[rewardCheckin] = v
	}
	var rewards *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "rewards" {
			rewards = root.Content[i+1]
			break
		}
	}
	if rewards == nil {
		return cfg, nil
	}
	if rewards.Kind == yaml.ScalarNode && rewards.Tag == "!!null" {
		return cfg, nil
	}
	if rewards.Kind != yaml.MappingNode {
		return cfg, errors.New("rewards must be a mapping")
	}
	// A rewards block is the current configuration contract. Its action
	// switches take precedence over the legacy growth_auto umbrella.
	cfg.Defaults.GrowthAuto = nil
	for i := 0; i+1 < len(rewards.Content); i += 2 {
		key, value := rewards.Content[i].Value, rewards.Content[i+1]
		switch key {
		case "auto":
			v, err := yamlBool(value, "rewards.auto")
			if err != nil {
				return cfg, err
			}
			cfg.Defaults.Auto = v
		case "actions":
			actions, err := parseRewardActions(value, "rewards.actions")
			if err != nil {
				return cfg, err
			}
			for action, enabled := range actions {
				cfg.Defaults.Actions[action] = enabled
			}
		case "accounts":
			if value.Kind != yaml.MappingNode {
				return cfg, errors.New("rewards.accounts must be a mapping")
			}
			for j := 0; j+1 < len(value.Content); j += 2 {
				accountKey := strings.TrimSpace(value.Content[j].Value)
				if accountKey == "" {
					return cfg, errors.New("rewards.accounts key must not be empty")
				}
				override, err := parseRewardOverride(value.Content[j+1], "rewards.accounts."+accountKey)
				if err != nil {
					return cfg, err
				}
				cfg.Accounts[accountKey] = override
			}
		default:
			return cfg, fmt.Errorf("unknown rewards key %q", key)
		}
	}
	return cfg, nil
}

func parseRewardOverride(node *yaml.Node, path string) (rewardOverride, error) {
	out := rewardOverride{Actions: map[rewardAction]*bool{}}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return out, nil
	}
	if node.Kind != yaml.MappingNode {
		return out, errors.New(path + " must be a mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "auto":
			v, err := yamlBool(value, path+".auto")
			if err != nil {
				return out, err
			}
			out.Auto = &v
		case "actions":
			actions, err := parseRewardActions(value, path+".actions")
			if err != nil {
				return out, err
			}
			for action, enabled := range actions {
				v := enabled
				out.Actions[action] = &v
			}
		default:
			return out, fmt.Errorf("unknown %s key %q", path, key)
		}
	}
	return out, nil
}

func parseRewardActions(node *yaml.Node, path string) (map[rewardAction]bool, error) {
	if node.Kind != yaml.MappingNode {
		return nil, errors.New(path + " must be a mapping")
	}
	out := map[rewardAction]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		action, ok := validRewardAction(node.Content[i].Value)
		if !ok {
			return nil, fmt.Errorf("unknown reward action %q in %s", node.Content[i].Value, path)
		}
		v, err := yamlBool(node.Content[i+1], path+"."+string(action))
		if err != nil {
			return nil, err
		}
		out[action] = v
	}
	return out, nil
}

func yamlBool(node *yaml.Node, path string) (bool, error) {
	if node.Kind != yaml.ScalarNode {
		return false, errors.New(path + " must be a boolean")
	}
	if node.Tag == "!!null" {
		return false, errors.New(path + " must be a boolean")
	}
	switch strings.ToLower(strings.TrimSpace(node.Value)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, errors.New(path + " must be a boolean")
	}
}

// handleRewardConfig updates the in-memory configuration for callers that use
// the plugin API directly. The panel saves through the host's generic plugin
// config endpoint so changes survive restart; this route reports persistent=false.
func handleRewardConfig(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Auto       *bool            `json:"auto"`
		Actions    map[string]*bool `json:"actions"`
		AccountKey string           `json:"account_key"`
	}
	if len(strings.TrimSpace(string(req.Body))) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return map[string]any{"error": "invalid JSON"}
		}
	}
	rewardConfigState.Lock()
	cfg := rewardConfigState.cfg
	cfg.Accounts = map[string]rewardOverride{}
	for key, override := range rewardConfigState.cfg.Accounts {
		cfg.Accounts[key] = override
	}
	if strings.TrimSpace(body.AccountKey) == "" {
		if body.Auto != nil {
			cfg.Defaults.Auto = *body.Auto
		}
		for name, enabled := range body.Actions {
			action, ok := validRewardAction(name)
			if !ok {
				rewardConfigState.Unlock()
				return map[string]any{"error": fmt.Sprintf("unknown reward action %q", name)}
			}
			if enabled != nil {
				cfg.Defaults.Actions[action] = *enabled
			}
		}
	} else {
		override := cfg.Accounts[body.AccountKey]
		if override.Actions == nil {
			override.Actions = map[rewardAction]*bool{}
		}
		if body.Auto != nil {
			override.Auto = body.Auto
		}
		for name, enabled := range body.Actions {
			action, ok := validRewardAction(name)
			if !ok {
				rewardConfigState.Unlock()
				return map[string]any{"error": fmt.Sprintf("unknown reward action %q", name)}
			}
			override.Actions[action] = enabled
		}
		cfg.Accounts[body.AccountKey] = override
	}
	rewardConfigState.cfg = cfg
	rewardConfigState.Unlock()
	out := rewardConfigJSON()
	out["persistent"] = false
	return out
}
