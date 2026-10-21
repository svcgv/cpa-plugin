package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func defaultRewardConfig() rewardConfig {
	return rewardConfig{
		Defaults: rewardSettings{Auto: true, Actions: map[rewardAction]bool{rewardCheckin: true}},
		Accounts: map[string]rewardOverride{},
	}
}

func parseRewardConfig(root *yaml.Node) (rewardConfig, error) {
	cfg := defaultRewardConfig()
	if root == nil {
		return cfg, nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "checkin_auto" {
			continue
		}
		value, err := yamlConfigBool(root.Content[i+1], "checkin_auto")
		if err != nil {
			return cfg, err
		}
		cfg.Defaults.Actions[rewardCheckin] = value
	}
	var rewards *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "rewards" {
			rewards = root.Content[i+1]
			break
		}
	}
	if rewards == nil || (rewards.Kind == yaml.ScalarNode && rewards.Tag == "!!null") {
		return cfg, nil
	}
	if rewards.Kind != yaml.MappingNode {
		return cfg, errors.New("rewards must be a mapping")
	}
	for i := 0; i+1 < len(rewards.Content); i += 2 {
		switch rewards.Content[i].Value {
		case "auto":
			value, err := yamlConfigBool(rewards.Content[i+1], "rewards.auto")
			if err != nil {
				return cfg, err
			}
			cfg.Defaults.Auto = value
		case "actions":
			actions, err := parseRewardActions(rewards.Content[i+1], "rewards.actions")
			if err != nil {
				return cfg, err
			}
			for action, enabled := range actions {
				cfg.Defaults.Actions[action] = enabled
			}
		case "accounts":
			node := rewards.Content[i+1]
			if node.Kind != yaml.MappingNode {
				return cfg, errors.New("rewards.accounts must be a mapping")
			}
			for j := 0; j+1 < len(node.Content); j += 2 {
				key := strings.TrimSpace(node.Content[j].Value)
				if key == "" {
					return cfg, errors.New("rewards.accounts key must not be empty")
				}
				override, err := parseRewardOverride(node.Content[j+1], "rewards.accounts."+key)
				if err != nil {
					return cfg, err
				}
				cfg.Accounts[key] = override
			}
		default:
			return cfg, fmt.Errorf("unknown rewards key %q", rewards.Content[i].Value)
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
		switch node.Content[i].Value {
		case "auto":
			value, err := yamlConfigBool(node.Content[i+1], path+".auto")
			if err != nil {
				return out, err
			}
			out.Auto = &value
		case "actions":
			actions, err := parseRewardActions(node.Content[i+1], path+".actions")
			if err != nil {
				return out, err
			}
			for action, enabled := range actions {
				v := enabled
				out.Actions[action] = &v
			}
		default:
			return out, fmt.Errorf("unknown %s key %q", path, node.Content[i].Value)
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
		value, err := yamlConfigBool(node.Content[i+1], path+"."+string(action))
		if err != nil {
			return nil, err
		}
		out[action] = value
	}
	return out, nil
}

func yamlConfigBool(node *yaml.Node, path string) (bool, error) {
	if node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
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

func rewardConfigJSON() map[string]any {
	rewardConfigState.RLock()
	defer rewardConfigState.RUnlock()
	return rewardConfigJSONLocked(rewardConfigState.cfg)
}

func rewardConfigJSONLocked(cfg rewardConfig) map[string]any {
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
			values := map[string]bool{}
			for action, enabled := range override.Actions {
				if enabled != nil {
					values[string(action)] = *enabled
				}
			}
			row["actions"] = values
		}
		accounts[key] = row
	}
	return map[string]any{"auto": cfg.Defaults.Auto, "actions": actions, "accounts": accounts}
}

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
