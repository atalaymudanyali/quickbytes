package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

// --- The public view ----------------------------------------------------------

// handleMenu answers GET /restaurants/{id}/menu for anyone, logged in or not.
// Archived categories and items are left out; sold-out items are included with
// "available": false, so the app can show them greyed out.
func (s *Server) handleMenu(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	m, err := s.menus.Menu(r.Context(), restaurantID, menus.View{})
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// --- The owner's view ---------------------------------------------------------

// handleOwnerMenu returns the whole menu, archived categories and items included
// (with archived_at set), so the owner can see and restore them.
func (s *Server) handleOwnerMenu(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	m, err := s.menus.Menu(r.Context(), restaurantID, menus.View{OwnerProfileID: owner})
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// --- Categories ---------------------------------------------------------------

type categoryRequest struct {
	Name     *string `json:"name"`
	Archived *bool   `json:"archived"`
}

func (s *Server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	var req categoryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := validateCategory(&req)
	if req.Name == nil {
		fields["name"] = "is required"
	}
	if req.Archived != nil {
		fields["archived"] = "can only be changed after creating the category"
	}
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	c, err := s.menus.CreateCategory(r.Context(), restaurantID, owner, *req.Name)
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleUpdateCategory(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	categoryID, ok := pathID(w, r, "category_id", "category_not_found", "no such category")
	if !ok {
		return
	}
	var req categoryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := validateCategory(&req)
	if req == (categoryRequest{}) {
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
	c, err := s.menus.UpdateCategory(r.Context(), restaurantID, owner, categoryID,
		menus.CategoryPatch{Name: req.Name, Archived: req.Archived})
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func validateCategory(req *categoryRequest) map[string]string {
	fields := map[string]string{}
	if req.Name != nil {
		*req.Name = strings.TrimSpace(*req.Name)
		if n := utf8.RuneCountInString(*req.Name); n < 1 || n > 60 {
			fields["name"] = "must be 1 to 60 characters"
		}
	}
	return fields
}

// --- Items --------------------------------------------------------------------

type itemRequest struct {
	CategoryID  *uuid.UUID `json:"category_id"`
	Name        *string    `json:"name"`
	Description *string    `json:"description"`
	PriceKurus  *int       `json:"price_kurus"`
	Available   *bool      `json:"available"`
	Archived    *bool      `json:"archived"`
}

func (s *Server) handleCreateItem(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	var req itemRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := validateItem(&req)
	if req.CategoryID == nil {
		fields["category_id"] = "is required"
	}
	if req.Name == nil {
		fields["name"] = "is required"
	}
	if req.PriceKurus == nil {
		fields["price_kurus"] = "is required"
	}
	if req.Available != nil || req.Archived != nil {
		fields["body"] = "available and archived can only be changed after creating the item"
	}
	if len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	description := ""
	if req.Description != nil {
		description = *req.Description
	}
	it, err := s.menus.CreateItem(r.Context(), restaurantID, owner, menus.NewItem{
		CategoryID: *req.CategoryID, Name: *req.Name, Description: description, PriceKurus: *req.PriceKurus,
	})
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, it)
}

func (s *Server) handleUpdateItem(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	itemID, ok := pathID(w, r, "item_id", "item_not_found", "no such menu item")
	if !ok {
		return
	}
	var req itemRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := validateItem(&req)
	if req == (itemRequest{}) {
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
	it, err := s.menus.UpdateItem(r.Context(), restaurantID, owner, itemID, menus.ItemPatch{
		CategoryID: req.CategoryID, Name: req.Name, Description: req.Description,
		PriceKurus: req.PriceKurus, Available: req.Available, Archived: req.Archived,
	})
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func validateItem(req *itemRequest) map[string]string {
	fields := map[string]string{}
	if req.Name != nil {
		*req.Name = strings.TrimSpace(*req.Name)
		if n := utf8.RuneCountInString(*req.Name); n < 1 || n > 100 {
			fields["name"] = "must be 1 to 100 characters"
		}
	}
	if req.Description != nil {
		*req.Description = strings.TrimSpace(*req.Description)
		if utf8.RuneCountInString(*req.Description) > 500 {
			fields["description"] = "must be at most 500 characters"
		}
	}
	if req.PriceKurus != nil && (*req.PriceKurus < 1 || *req.PriceKurus > menus.MaxPriceKurus) {
		fields["price_kurus"] = fmt.Sprintf("must be 1 to %d kuruş (0.01 to 100,000 TL)", menus.MaxPriceKurus)
	}
	return fields
}

// --- Shared helpers -----------------------------------------------------------

// writeMenuError turns the store's errors into responses. A category that can't
// be used for an item (another restaurant's, archived, or unknown) is a problem
// with the request's category_id, so it's a validation error, not a 404.
func (s *Server) writeMenuError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, menus.ErrRestaurantNotFound):
		writeRestaurantNotFound(w, r)
	case errors.Is(err, menus.ErrItemNotFound):
		writeError(w, r, http.StatusNotFound, "item_not_found", "no such menu item")
	case errors.Is(err, menus.ErrCategoryNotFound) && r.PathValue("category_id") != "":
		writeError(w, r, http.StatusNotFound, "category_not_found", "no such category")
	case errors.Is(err, menus.ErrCategoryNotFound):
		writeValidationError(w, r, map[string]string{"category_id": "must be an active category of this restaurant"})
	default:
		s.writeInternalError(w, r, err)
	}
}

// pathID reads a UUID from the URL. Anything that isn't a UUID can't exist, so it
// gets the same 404 as an unknown ID.
func pathID(w http.ResponseWriter, r *http.Request, name, code, message string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		writeError(w, r, http.StatusNotFound, code, message)
		return uuid.Nil, false
	}
	return id, true
}

// --- Reordering ---------------------------------------------------------------

// maxOrderLen caps how many IDs one reorder request may list. A real menu has a
// few dozen; the cap keeps a malicious request from sending millions.
const maxOrderLen = 500

type categoryOrderRequest struct {
	CategoryIDs []uuid.UUID `json:"category_ids"`
}

type itemOrderRequest struct {
	ItemIDs []uuid.UUID `json:"item_ids"`
}

// handleReorderCategories answers PUT /restaurants/{id}/menu/categories/order.
// PUT because the request replaces the whole order: sending it twice has the same
// effect as sending it once.
func (s *Server) handleReorderCategories(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	var req categoryOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.CategoryIDs) == 0 || len(req.CategoryIDs) > maxOrderLen {
		writeValidationError(w, r, map[string]string{"category_ids": fmt.Sprintf("must list 1 to %d categories", maxOrderLen)})
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	err := s.menus.ReorderCategories(r.Context(), restaurantID, owner, req.CategoryIDs)
	s.respondWithOwnerMenu(w, r, restaurantID, owner, err, "category_ids")
}

// handleReorderItems answers PUT /restaurants/{id}/menu/categories/{category_id}/order.
func (s *Server) handleReorderItems(w http.ResponseWriter, r *http.Request) {
	restaurantID, ok := restaurantID(w, r)
	if !ok {
		return
	}
	categoryID, ok := pathID(w, r, "category_id", "category_not_found", "no such category")
	if !ok {
		return
	}
	var req itemOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.ItemIDs) == 0 || len(req.ItemIDs) > maxOrderLen {
		writeValidationError(w, r, map[string]string{"item_ids": fmt.Sprintf("must list 1 to %d items", maxOrderLen)})
		return
	}
	owner, ok := s.restaurantProfile(w, r)
	if !ok {
		return
	}
	err := s.menus.ReorderItems(r.Context(), restaurantID, owner, categoryID, req.ItemIDs)
	s.respondWithOwnerMenu(w, r, restaurantID, owner, err, "item_ids")
}

// respondWithOwnerMenu finishes a reorder: on success it returns the owner's menu
// in its new order, so the app can redraw without a second request.
func (s *Server) respondWithOwnerMenu(w http.ResponseWriter, r *http.Request, restaurantID, owner uuid.UUID, err error, field string) {
	if errors.Is(err, menus.ErrOrderMismatch) {
		writeValidationError(w, r, map[string]string{field: "must list every active entry exactly once, with nothing extra"})
		return
	}
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	m, err := s.menus.Menu(r.Context(), restaurantID, menus.View{OwnerProfileID: owner})
	if err != nil {
		s.writeMenuError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}
