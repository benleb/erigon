// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package diagnostics

import (
	"context"
	"fmt"

	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/log/v3"
)

var (
	StagesListKey   = []byte("diagStagesList")
	CurrentStageKey = []byte("diagCurrentStage")
)

type CurrentSyncStagesIdxs struct {
	Stage    int `json:"currentStage"`
	SubStage int `json:"currentSubStage"`
}

type SyncStage struct {
	ID        string         `json:"id"`
	State     StageState     `json:"state"`
	SubStages []SyncSubStage `json:"subStages"`
	Stats     SyncStageStats `json:"stats"`
}

type SyncSubStage struct {
	ID    string         `json:"id"`
	State StageState     `json:"state"`
	Stats SyncStageStats `json:"stats"`
}

type SyncStageStats struct {
	TimeElapsed string `json:"timeElapsed"`
	TimeLeft    string `json:"timeLeft"`
	Progress    string `json:"progress"`
}

type SetSyncSubStageList struct {
	Stage string
	List  []SyncSubStage
}

func (ti SetSyncSubStageList) Type() Type {
	return TypeOf(ti)
}

type SyncStageList struct {
	StagesList []SyncStage `json:"stages"`
}

func (ti SyncStageList) Type() Type {
	return TypeOf(ti)
}

type StageState int

const (
	Queued StageState = iota
	Running
	Completed
)

func (s StageState) String() string {
	return [...]string{"Queued", "Running", "Completed"}[s]
}

type CurrentSyncStage struct {
	Stage string `json:"stage"`
}

func (ti CurrentSyncStage) Type() Type {
	return TypeOf(ti)
}

type CurrentSyncSubStage struct {
	SubStage string `json:"subStage"`
}

func (ti CurrentSyncSubStage) Type() Type {
	return TypeOf(ti)
}

func (d *DiagnosticClient) setupStagesDiagnostics(rootCtx context.Context) {
	d.runSyncStagesListListener(rootCtx)
	d.runCurrentSyncStageListener(rootCtx)
	d.runCurrentSyncSubStageListener(rootCtx)
	d.runSubStageListener(rootCtx)
}

func (d *DiagnosticClient) runSyncStagesListListener(rootCtx context.Context) {
	go func() {
		ctx, ch, closeChannel := Context[SyncStageList](rootCtx, 1)
		defer closeChannel()

		StartProviders(ctx, TypeOf(SyncStageList{}), log.Root())
		for {
			select {
			case <-rootCtx.Done():
				return
			case info := <-ch:
				func() {
					d.mu.Lock()
					defer d.mu.Unlock()
					d.SetStagesList(info.StagesList)
					d.saveSyncStagesToDB()
				}()
			}
		}
	}()
}

func (d *DiagnosticClient) runCurrentSyncStageListener(rootCtx context.Context) {
	go func() {
		ctx, ch, closeChannel := Context[CurrentSyncStage](rootCtx, 1)
		defer closeChannel()

		StartProviders(ctx, TypeOf(CurrentSyncStage{}), log.Root())
		for {
			select {
			case <-rootCtx.Done():
				return
			case info := <-ch:

				err := d.SetCurrentSyncStage(info)
				if err != nil {
					log.Warn("[Diagnostics] Failed to set current stage", "err", err)
				}

				d.saveSyncStagesToDB()
			}
		}
	}()
}

func (d *DiagnosticClient) runCurrentSyncSubStageListener(rootCtx context.Context) {
	go func() {
		ctx, ch, closeChannel := Context[CurrentSyncSubStage](rootCtx, 1)
		defer closeChannel()

		StartProviders(ctx, TypeOf(CurrentSyncSubStage{}), log.Root())
		for {
			select {
			case <-rootCtx.Done():
				return
			case info := <-ch:
				d.mu.Lock()
				d.SetCurrentSyncSubStage(info)
				d.mu.Unlock()

				d.saveSyncStagesToDB()
			}
		}
	}()
}

func (d *DiagnosticClient) runSubStageListener(rootCtx context.Context) {
	go func() {
		ctx, ch, closeChannel := Context[SetSyncSubStageList](rootCtx, 1)
		defer closeChannel()

		StartProviders(ctx, TypeOf(SetSyncSubStageList{}), log.Root())
		for {
			select {
			case <-rootCtx.Done():
				return
			case info := <-ch:
				d.mu.Lock()
				d.SetSubStagesList(info.Stage, info.List)
				d.mu.Unlock()

				d.saveSyncStagesToDB()
			}
		}
	}()
}

func (d *DiagnosticClient) saveSyncStagesToDB() {
	if err := d.db.Update(d.ctx, StagesListUpdater(d.syncStages)); err != nil {
		log.Error("[Diagnostics] Failed to update stages list", "err", err)
	}
}

func (d *DiagnosticClient) GetCurrentSyncIdxs() CurrentSyncStagesIdxs {
	currentIdxs := CurrentSyncStagesIdxs{
		Stage:    -1,
		SubStage: -1,
	}

	for sIdx, stage := range d.syncStages {
		if stage.State == Running {
			currentIdxs.Stage = sIdx

			for subIdx, subStage := range stage.SubStages {
				if subStage.State == Running {
					currentIdxs.SubStage = subIdx
				}
			}
			break
		}
	}

	return currentIdxs
}

func (d *DiagnosticClient) SetStagesList(stages []SyncStage) {
	if len(d.syncStages) != len(stages) {
		d.syncStages = stages
	}
}

func (d *DiagnosticClient) SetSubStagesList(stageId string, subStages []SyncSubStage) {
	for idx, stage := range d.syncStages {
		if stage.ID == stageId {
			if len(d.syncStages[idx].SubStages) != len(subStages) {
				d.syncStages[idx].SubStages = subStages
				break
			}
		}
	}
}

func (d *DiagnosticClient) SetCurrentSyncStage(css CurrentSyncStage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	stageState, err := d.GetStageState(css.Stage)
	if err != nil {
		return err
	}

	if stageState == Completed {
		return nil
	}

	isSet := false
	for idx, stage := range d.syncStages {
		if !isSet {
			if stage.ID == css.Stage {
				d.syncStages[idx].State = Running
				isSet = true
			} else {
				d.setStagesState(idx, Completed)
			}
		} else {
			d.setStagesState(idx, Queued)
		}
	}

	return nil
}

func (d *DiagnosticClient) setStagesState(stadeIdx int, state StageState) {
	d.syncStages[stadeIdx].State = state
	d.setSubStagesState(stadeIdx, state)
}

func (d *DiagnosticClient) setSubStagesState(stadeIdx int, state StageState) {
	for subIdx := range d.syncStages[stadeIdx].SubStages {
		d.syncStages[stadeIdx].SubStages[subIdx].State = state
	}
}

func (d *DiagnosticClient) SetCurrentSyncSubStage(css CurrentSyncSubStage) {
	for idx, stage := range d.syncStages {
		if stage.State == Running {
			for subIdx, subStage := range stage.SubStages {
				if subStage.ID == css.SubStage {
					if d.syncStages[idx].SubStages[subIdx].State == Completed {
						return
					}

					if subIdx > 0 {
						d.syncStages[idx].SubStages[subIdx-1].State = Completed
					}

					d.syncStages[idx].SubStages[subIdx].State = Running
				}
			}

			break
		}
	}
}

func (d *DiagnosticClient) GetStageState(stageId string) (StageState, error) {
	for _, stage := range d.syncStages {
		if stage.ID == stageId {
			return stage.State, nil
		}
	}

	stagesIdsList := make([]string, 0, len(d.syncStages))
	for _, stage := range d.syncStages {
		stagesIdsList = append(stagesIdsList, stage.ID)
	}

	return 0, fmt.Errorf("stage %s not found in stages list %s", stageId, stagesIdsList)
}

func SyncStagesFromTX(tx kv.Tx) ([]byte, error) {
	bytes, err := ReadDataFromTable(tx, kv.DiagSyncStages, StagesListKey)
	if err != nil {
		return nil, err
	}

	return common.CopyBytes(bytes), nil
}

func StagesListUpdater(info []SyncStage) func(tx kv.RwTx) error {
	return PutDataToTable(kv.DiagSyncStages, StagesListKey, info)
}

func (d *DiagnosticClient) GetSyncStages() []SyncStage {
	return d.syncStages
}
