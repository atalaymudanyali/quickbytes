package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

// --- Option groups ------------------------------------------------------------

type optionGroupRequest struct {
	Name     *string `json:"name"`
	MinPicks *int    `json:"min_picks"`
	MaxPicks *int    `json:"max_picks"`
}

// handleCreateOptionGroup answers POST /restaurants/{id}/menu/items/{item_id}/option-groups.
func (s *Server) handleCreateOptionGroup(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	itemID, ok := pathID(w, r, "item_id", "item_not_found", "no such menu item")
	if !ok {
		return
	}
	var req optionGroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := validateOptionGroup(&req)
	if req.Name == nil {
		fields["name"] = "is required"
	}
	// Both required: "how many may be picked" has no safe default.
	if req.MinPicks == nil {
		fields["min_picks"] = "is required"
	}
	if req.MaxPicks == nil {
		fields["max_picks"] = "is required"
	}
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	g, err := s.menus.CreateOptionGroup(r.Context(), restaurantID, owner, itemID, menus.NewOptionGroup{
		Name: *req.Name, MinPicks: *req.MinPicks, MaxPicks: *req.MaxPicks,
	})
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) handleUpdateOptionGroup(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	groupID, ok := pathID(w, r, "group_id", "option_group_not_found", "no such option group")
	if !ok {
		return
	}
	var req optionGroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := validateOptionGroup(&req)
	if req == (optionGroupRequest{}) {
		fields["body"] = "nothing to update"
	}
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	g, err := s.menus.UpdateOptionGroup(r.Context(), restaurantID, owner, groupID, menus.OptionGroupPatch{
		Name: req.Name, MinPicks: req.MinPicks, MaxPicks: req.MaxPicks,
	})
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// handleDeleteOptionGroup deletes a group with its choices. 204 No Content: done,
// and nothing left to send back.
func (s *Server) handleDeleteOptionGroup(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	groupID, ok := pathID(w, r, "group_id", "option_group_not_found", "no such option group")
	if !ok {
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	if err := s.menus.DeleteOptionGroup(r.Context(), restaurantID, owner, groupID); err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateOptionGroup checks each sent field on its own, and min <= max when
// both are sent. When a patch sends only one, the database compares it with the
// stored other (menus.ErrInvalidPicks).
func validateOptionGroup(req *optionGroupRequest) map[string]string {
	fields := map[string]string{}
	if req.Name != nil {
		*req.Name = strings.TrimSpace(*req.Name)
		if n := utf8.RuneCountInString(*req.Name); n < 1 || n > 60 {
			fields["name"] = "must be 1 to 60 characters"
		}
	}
	if req.MinPicks != nil && (*req.MinPicks < 0 || *req.MinPicks > menus.MaxPicks) {
		fields["min_picks"] = fmt.Sprintf("must be 0 to %d", menus.MaxPicks)
	}
	if req.MaxPicks != nil && (*req.MaxPicks < 1 || *req.MaxPicks > menus.MaxPicks) {
		fields["max_picks"] = fmt.Sprintf("must be 1 to %d", menus.MaxPicks)
	}
	if len(fields) == 0 && req.MinPicks != nil && req.MaxPicks != nil && *req.MinPicks > *req.MaxPicks {
		fields["min_picks"] = "must not be above max_picks"
	}
	return fields
}

// --- Option choices -----------------------------------------------------------

type optionChoiceRequest struct {
	Name            *string `json:"name"`
	ExtraPriceKurus *int    `json:"extra_price_kurus"`
	Available       *bool   `json:"available"`
}

// handleCreateOptionChoice answers POST /restaurants/{id}/menu/option-groups/{group_id}/choices.
func (s *Server) handleCreateOptionChoice(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	groupID, ok := pathID(w, r, "group_id", "option_group_not_found", "no such option group")
	if !ok {
		return
	}
	var req optionChoiceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := validateOptionChoice(&req)
	if req.Name == nil {
		fields["name"] = "is required"
	}
	if req.Available != nil {
		fields["available"] = "can only be changed after creating the choice"
	}
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	// Not sent means no extra charge ("Small", "No sauce").
	extra := 0
	if req.ExtraPriceKurus != nil {
		extra = *req.ExtraPriceKurus
	}
	c, err := s.menus.CreateOptionChoice(r.Context(), restaurantID, owner, groupID, menus.NewOptionChoice{
		Name: *req.Name, ExtraPriceKurus: extra,
	})
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleUpdateOptionChoice(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	choiceID, ok := pathID(w, r, "choice_id", "option_choice_not_found", "no such option choice")
	if !ok {
		return
	}
	var req optionChoiceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := validateOptionChoice(&req)
	if req == (optionChoiceRequest{}) {
		fields["body"] = "nothing to update"
	}
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	c, err := s.menus.UpdateOptionChoice(r.Context(), restaurantID, owner, choiceID, menus.OptionChoicePatch{
		Name: req.Name, ExtraPriceKurus: req.ExtraPriceKurus, Available: req.Available,
	})
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleDeleteOptionChoice(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	choiceID, ok := pathID(w, r, "choice_id", "option_choice_not_found", "no such option choice")
	if !ok {
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	if err := s.menus.DeleteOptionChoice(r.Context(), restaurantID, owner, choiceID); err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateOptionChoice(req *optionChoiceRequest) map[string]string {
	fields := map[string]string{}
	if req.Name != nil {
		*req.Name = strings.TrimSpace(*req.Name)
		if n := utf8.RuneCountInString(*req.Name); n < 1 || n > 100 {
			fields["name"] = "must be 1 to 100 characters"
		}
	}
	if req.ExtraPriceKurus != nil && (*req.ExtraPriceKurus < 0 || *req.ExtraPriceKurus > menus.MaxPriceKurus) {
		fields["extra_price_kurus"] = fmt.Sprintf("must be 0 to %d kuruş (0 to 100,000 TL)", menus.MaxPriceKurus)
	}
	return fields
}
