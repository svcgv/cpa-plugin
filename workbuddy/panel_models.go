package main

import (
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var panelModelRefreshErrors sync.Map // auth index -> sanitized error

// The panel reads only snapshots belonging to each auth; it never exposes tokens.
func handlePanelModelStatus() map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": "账号列表获取失败"}
	}
	runtime := currentModelRuntime()
	accounts := make([]map[string]any, 0, len(files))
	for _, f := range files {
		row := map[string]any{"auth_index": f.AuthIndex, "name": f.Name, "state": modelNotStarted, "source": modelSourceNone, "models": []any{}}
		sa, getErr := hostAuthGet(f.AuthIndex)
		if getErr != nil || sa == nil {
			row["error"] = "账号读取失败"
			accounts = append(accounts, row)
			continue
		}
		row["region"] = serviceFor(sa)
		if sa.Account.Nickname != "" {
			row["name"] = sa.Account.Nickname
		}
		snap := runtime.snapshotForAuthID(f.ID)
		row["state"], row["source"], row["fetched_at"] = snap.State, snap.ModelSource, modelStatusTime(snap.ModelsFetchedAt)
		if snap.ErrorCode != modelErrorNone {
			row["error"] = snap.ErrorCode
		}
		rates := make(map[string]string, len(snap.CatalogFacts))
		for _, fact := range snap.CatalogFacts {
			rates[fact.ID] = fact.Credits
		}
		models := make([]map[string]any, 0, len(snap.Models))
		for _, model := range snap.Models {
			models = append(models, map[string]any{"id": model.ID, "name": model.Name, "credits": rates[model.ID]})
		}
		row["models"] = models
		if failure, ok := panelModelRefreshErrors.Load(f.AuthIndex); ok {
			row["error"] = failure
		}
		accounts = append(accounts, row)
	}
	return map[string]any{"accounts": accounts}
}

func refreshPanelModels(callbackID string) map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": "账号列表获取失败"}
	}
	runtime := currentModelRuntime()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, file := range files {
		f := file
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			physical, err := hostAuthGetPhysical(f.AuthIndex)
			if err != nil || physical == nil {
				panelModelRefreshErrors.Store(f.AuthIndex, "账号读取失败")
				return
			}
			panelModelRefreshErrors.Delete(f.AuthIndex)
			runtime.ensureForAuthRefresh(authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: f.ID, StorageJSON: physical.JSON}, HostCallbackID: callbackID}, true)
		}()
	}
	wg.Wait()
	return handlePanelModelStatus()
}
