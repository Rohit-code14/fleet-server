// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fleet/internal/pkg/bulk"
	"fleet/internal/pkg/dl"
	"fleet/internal/pkg/model"
	"fleet/internal/pkg/saved"

	"github.com/gofrs/uuid"
	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog/log"
)

var ErrEventAgentIdMismatch = errors.New("event agentId mismatch")

func (rt Router) handleAcks(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id := ps.ByName("id")

	err := _handleAcks(w, r, id, rt.sv, rt.ct.bulker)

	if err != nil {
		code := http.StatusBadRequest
		// Don't log connection drops
		if err != context.Canceled {
			log.Error().Err(err).Int("code", code).Msg("Fail ACK")
		}

		http.Error(w, err.Error(), code)
	}
}

// TODO: Handle UPGRADE and UNENROLL
func _handleAcks(w http.ResponseWriter, r *http.Request, id string, sv saved.CRUD, bulker bulk.Bulk) error {
	agent, err := authAgent(r, id, bulker)
	if err != nil {
		return err
	}

	raw, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}

	var req AckRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}

	log.Trace().RawJSON("raw", raw).Msg("Ack request")

	if err = _handleAckEvents(r.Context(), agent, req.Events, sv, bulker); err != nil {
		return err
	}

	// TODO: flesh this out
	resp := AckResponse{"acks"}

	data, err := json.Marshal(&resp)
	if err != nil {
		return err
	}

	if _, err = w.Write(data); err != nil {
		return err
	}

	return nil
}

func _handleAckEvents(ctx context.Context, agent *model.Agent, events []Event, sv saved.CRUD, bulker bulk.Bulk) error {

	// Retrieve each action
	m := map[string][]Action{}

	var policyAcks []string
	for _, ev := range events {
		if ev.AgentId != "" && ev.AgentId != agent.Id {
			return ErrEventAgentIdMismatch
		}
		if strings.HasPrefix(ev.ActionId, "policy:") {
			policyAcks = append(policyAcks, ev.ActionId)
			continue
		}

		action, ok := gCache.GetAction(ev.ActionId)
		if !ok {
			if err := sv.Read(ctx, AGENT_ACTION_SAVED_OBJECT_TYPE, ev.ActionId, &action); err != nil {
				return err
			}
		}

		// TODO: Handle not found diffently?  Ignore ACK?
		actionList := m[action.Type]
		m[action.Type] = append(actionList, action)
	}

	if policyAcks != nil {
		if err := _handlePolicyChange(ctx, bulker, agent, policyAcks...); err != nil {
			return err
		}
	}

	// TODO: handle UPGRADE and UNENROLL

	return nil
}

func _handlePolicyChange(ctx context.Context, bulker bulk.Bulk, agent *model.Agent, actionIds ...string) error {
	// If more than one, pick the winner;
	// 0) Correct policy id
	// 1) Highest revision/coordinator number

	found := false
	currRev := agent.PolicyRevisionIdx
	currCoord := agent.PolicyCoordinatorIdx
	for _, a := range actionIds {
		action, ok := parsePolicyAction(a)
		if ok && action.policyId == agent.PolicyId && (action.revIdx > currRev ||
			(action.revIdx == currRev && action.coordIdx > currCoord)) {
			found = true
			currRev = action.revIdx
			currCoord = action.coordIdx
		}
	}

	if found {
		updates := make([]bulk.BulkOp, 0, 1)
		fields := map[string]interface{}{
			dl.FieldPolicyRevisionIdx:    currRev,
			dl.FieldPolicyCoordinatorIdx: currCoord,
		}
		fields[dl.FieldUpdatedAt] = time.Now().UTC().Format(time.RFC3339)

		source, err := json.Marshal(map[string]interface{}{
			"doc": fields,
		})
		if err != nil {
			return err
		}

		updates = append(updates, bulk.BulkOp{
			Id:    agent.Id,
			Body:  source,
			Index: dl.FleetAgents,
		})

		err = bulker.MUpdate(ctx, updates, bulk.WithRefresh())
		if err != nil {
			return err
		}
	}

	return nil
}

type policyAction struct {
	policyId string
	revIdx   int64
	coordIdx int64
}

func parsePolicyAction(actionId string) (policyAction, bool) {
	split := strings.Split(actionId, ":")
	if len(split) != 4 {
		return policyAction{}, false
	}
	if split[0] != "policy" {
		return policyAction{}, false
	}
	if _, err := uuid.FromString(split[1]); err != nil {
		return policyAction{}, false
	}
	revIdx, err := strconv.Atoi(split[2])
	if err != nil {
		return policyAction{}, false
	}
	coordIdx, err := strconv.Atoi(split[3])
	if err != nil {
		return policyAction{}, false
	}
	return policyAction{
		policyId: split[1],
		revIdx:   int64(revIdx),
		coordIdx: int64(coordIdx),
	}, true
}
