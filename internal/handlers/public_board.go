package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"windshift/internal/blobstore"
	"windshift/internal/database"
	"windshift/internal/fileserve"
	"windshift/internal/markdown"
	"windshift/internal/models"
	"windshift/internal/repository"
	"windshift/internal/services"
)

const publicBoardItemLimit = 500

var publicBoardCardFieldAllowlist = map[string]struct{}{
	"key":          {},
	"title":        {},
	"status":       {},
	"priority":     {},
	"assignee":     {},
	"item_type":    {},
	"story_points": {},
	"due_date":     {},
	"labels":       {},
	"project":      {},
}

func filterPublicBoardCardFields(fields []models.ListColumn) []models.ListColumn {
	filtered := make([]models.ListColumn, 0, len(fields))
	for _, field := range fields {
		if field.FieldType != "system" {
			continue
		}
		if _, allowed := publicBoardCardFieldAllowlist[field.FieldIdentifier]; allowed {
			filtered = append(filtered, field)
		}
	}
	return filtered
}

func rewritePublicAttachmentURLs(content, slug string) string {
	return strings.ReplaceAll(
		content,
		"/api/attachments/",
		fmt.Sprintf("/api/public/board/%s/attachments/", slug),
	)
}

// PublicBoardHandler serves read-only public board views
type PublicBoardHandler struct {
	db                database.Database
	permissionService *services.PermissionService
	publicBoardScope  *services.PublicBoardScopeService
	storage           *blobstore.Store
}

// NewPublicBoardHandler creates a new PublicBoardHandler
func NewPublicBoardHandler(db database.Database, permissionService *services.PermissionService, storage *blobstore.Store) *PublicBoardHandler {
	return &PublicBoardHandler{
		db:                db,
		permissionService: permissionService,
		publicBoardScope:  services.NewPublicBoardScopeService(db, permissionService),
		storage:           storage,
	}
}

func isPublicBoardScopeError(err error) bool {
	return errors.Is(err, services.ErrPublicBoardWorkspaceScopeRequired) ||
		errors.Is(err, services.ErrPublicBoardWorkspaceNotFound)
}

// publicBoardCard is a stripped-down item for public display
type publicBoardCard struct {
	Key            string        `json:"key"`
	Title          string        `json:"title"`
	PriorityName   string        `json:"priority_name,omitempty"`
	PriorityIcon   string        `json:"priority_icon,omitempty"`
	PriorityColor  string        `json:"priority_color,omitempty"`
	AssigneeName   string        `json:"assignee_name,omitempty"`
	AssigneeAvatar string        `json:"assignee_avatar,omitempty"`
	Labels         []publicLabel `json:"labels,omitempty"`
	MilestoneIDs   []int         `json:"milestone_ids,omitempty"`
	DueDate        string        `json:"due_date,omitempty"`
	StatusID       *int          `json:"status_id,omitempty"`
	StatusName     string        `json:"status_name,omitempty"`
	ItemTypeName   string        `json:"item_type_name,omitempty"`
	StoryPoints    *float64      `json:"story_points,omitempty"`
	ProjectID      *int          `json:"project_id,omitempty"`
	ProjectName    string        `json:"project_name,omitempty"`
	// LaneKey places the card in a grouped public board lane
	// ("assignee-3", "status-12", "project-5" or "none").
	LaneKey string `json:"lane_key,omitempty"`

	// Customer derived from the card's effective project; unexported so the
	// lane builder can group by customer without serializing it per card.
	customerID   *int
	customerName string
}

// publicBoardLane is one swimlane header on a grouped public board. Customer
// grouping nests project lanes under their customer via Children.
type publicBoardLane struct {
	Key      string            `json:"key"`
	Title    string            `json:"title"`
	Children []publicBoardLane `json:"children,omitempty"`
}

type publicMilestone struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	TargetDate    string `json:"target_date,omitempty"`
	Status        string `json:"status,omitempty"`
	CategoryColor string `json:"category_color,omitempty"`
}

type publicLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type publicColumn struct {
	Name     string            `json:"name"`
	Color    string            `json:"color"`
	WIPLimit *int              `json:"wip_limit,omitempty"`
	Items    []publicBoardCard `json:"items"`
}

type publicBoardResponse struct {
	Collection struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"collection"`
	Columns    []publicColumn      `json:"columns"`
	CardFields []models.ListColumn `json:"card_fields,omitempty"`
	Swimlanes  bool                `json:"swimlanes"`
	Milestones []publicMilestone   `json:"milestones,omitempty"`
	// GroupBy is the configured grouping ("", "milestone", "assignee",
	// "status" or "project"); Lanes carries the ordered lane headers for the
	// non-milestone dimensions.
	GroupBy     string            `json:"group_by,omitempty"`
	Lanes       []publicBoardLane `json:"lanes,omitempty"`
	TotalItems  int               `json:"total_items"`
	LoadedItems int               `json:"loaded_items"`
	Truncated   bool              `json:"truncated"`
	ItemLimit   int               `json:"item_limit"`
	UpdatedAt   string            `json:"updated_at"`
}

type publicComment struct {
	AuthorName   string `json:"author_name"`
	AuthorAvatar string `json:"author_avatar,omitempty"`
	Content      string `json:"content"`
	ContentHTML  string `json:"content_html,omitempty"`
	CreatedAt    string `json:"created_at"`
}

type publicItemDetail struct {
	Key             string          `json:"key"`
	Title           string          `json:"title"`
	Description     string          `json:"description"`
	DescriptionHTML string          `json:"description_html,omitempty"`
	StatusName      string          `json:"status_name,omitempty"`
	StatusColor     string          `json:"status_color,omitempty"`
	PriorityName    string          `json:"priority_name,omitempty"`
	PriorityIcon    string          `json:"priority_icon,omitempty"`
	PriorityColor   string          `json:"priority_color,omitempty"`
	ItemTypeName    string          `json:"item_type_name,omitempty"`
	ItemTypeIcon    string          `json:"item_type_icon,omitempty"`
	ItemTypeColor   string          `json:"item_type_color,omitempty"`
	AssigneeName    string          `json:"assignee_name,omitempty"`
	AssigneeAvatar  string          `json:"assignee_avatar,omitempty"`
	DueDate         string          `json:"due_date,omitempty"`
	Labels          []publicLabel   `json:"labels,omitempty"`
	StoryPoints     *float64        `json:"story_points,omitempty"`
	Comments        []publicComment `json:"comments"`
	CreatedAt       string          `json:"created_at"`
}

// GetPublicBoardItem serves a single item detail for a public collection
func (h *PublicBoardHandler) GetPublicBoardItem(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	key := r.PathValue("key")
	if slug == "" || key == "" {
		respondNotFound(w, r, "item")
		return
	}

	// Split key on last "-" → workspace key + item number
	lastDash := strings.LastIndex(key, "-")
	if lastDash <= 0 {
		respondNotFound(w, r, "item")
		return
	}
	workspaceKey := key[:lastDash]
	itemNumber, err := strconv.Atoi(key[lastDash+1:])
	if err != nil || itemNumber <= 0 {
		respondNotFound(w, r, "item")
		return
	}

	// Validate slug → public collection
	var collectionID int
	err = h.db.QueryRow(`
		SELECT id FROM collections
		WHERE public_slug = ? AND is_public = true AND is_private = false AND public_slug IS NOT NULL
	`, slug).Scan(&collectionID)
	if errors.Is(err, sql.ErrNoRows) {
		respondNotFound(w, r, "board")
		return
	}
	if err != nil {
		respondInternalError(w, r, err)
		return
	}

	publicItem, err := repository.NewItemRepository(h.db).FindPublicItemByKeyAndNumber(workspaceKey, itemNumber)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			respondNotFound(w, r, "item")
			return
		}
		respondInternalError(w, r, err)
		return
	}
	itemID := publicItem.ID
	title := publicItem.Title
	description := publicItem.Description
	createdAt := publicItem.CreatedAt

	// Verify item belongs to this collection by running the collection query
	belongs, err := h.itemBelongsToCollection(itemID, collectionID)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}
	if !belongs {
		respondNotFound(w, r, "item")
		return
	}

	// Load labels for this item
	labels := h.loadSingleItemLabels(itemID)

	// Load public comments (non-private only)
	comments, err := h.loadPublicComments(itemID, slug)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}

	// Rewrite attachment URLs for public access
	description = rewritePublicAttachmentURLs(description, slug)
	descriptionHTML, err := markdown.Render(description)
	if err != nil {
		respondInternalError(w, r, fmt.Errorf("render public item description: %w", err))
		return
	}
	for i := range comments {
		comments[i].ContentHTML, err = markdown.Render(comments[i].Content)
		if err != nil {
			respondInternalError(w, r, fmt.Errorf("render public comment: %w", err))
			return
		}
	}

	detail := publicItemDetail{
		Key:             key,
		Title:           title,
		Description:     description,
		DescriptionHTML: descriptionHTML,
		StatusName:      publicItem.StatusName,
		StatusColor:     publicItem.StatusColor,
		PriorityName:    publicItem.PriorityName,
		PriorityIcon:    publicItem.PriorityIcon,
		PriorityColor:   publicItem.PriorityColor,
		ItemTypeName:    publicItem.ItemTypeName,
		ItemTypeIcon:    publicItem.ItemTypeIcon,
		ItemTypeColor:   publicItem.ItemTypeColor,
		AssigneeName:    publicItem.AssigneeName,
		AssigneeAvatar:  publicItem.AssigneeAvatar,
		Labels:          labels,
		Comments:        comments,
		CreatedAt:       createdAt,
		DueDate:         publicItem.DueDate,
		StoryPoints:     publicItem.StoryPoints,
	}

	respondJSONOK(w, detail)
}

// GetPublicBoard serves a read-only board view for a public collection
func (h *PublicBoardHandler) GetPublicBoard(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		respondNotFound(w, r, "board")
		return
	}

	// Look up collection by public_slug where is_public = true
	var collectionID int
	var collectionName, collectionDescription, qlQuery, updatedAt string
	var publicMilestoneSwimlanes bool
	var publicGroupBy string
	err := h.db.QueryRow(`
		SELECT id, name, COALESCE(description, ''), COALESCE(ql_query, ''), public_milestone_swimlanes, public_group_by, updated_at
		FROM collections
		WHERE public_slug = ? AND is_public = true AND is_private = false AND public_slug IS NOT NULL
	`, slug).Scan(&collectionID, &collectionName, &collectionDescription, &qlQuery, &publicMilestoneSwimlanes, &publicGroupBy, &updatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		respondNotFound(w, r, "board")
		return
	}
	if err != nil {
		respondInternalError(w, r, err)
		return
	}

	// Explicit grouping wins; boards shared before the column existed keep the
	// legacy milestone flag. Unknown stored values degrade to a flat board.
	effectiveGroupBy := publicGroupBy
	switch effectiveGroupBy {
	case "", "milestone", "assignee", "status", "project":
	default:
		effectiveGroupBy = ""
	}
	if effectiveGroupBy == "" && publicMilestoneSwimlanes {
		effectiveGroupBy = "milestone"
	}

	// Load board configuration for this collection
	var boardConfigID int
	var backlogStatusIDsJSON, cardFieldsJSON sql.NullString
	err = h.db.QueryRow(`
		SELECT id, backlog_status_ids, card_fields
		FROM board_configurations
		WHERE collection_id = ?
	`, collectionID).Scan(&boardConfigID, &backlogStatusIDsJSON, &cardFieldsJSON)

	var cardFields []models.ListColumn
	var backlogStatusSet map[int]bool
	var columns []boardColumnInfo

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No explicit board config — build default columns from status categories
		columns, err = h.buildDefaultColumns()
		if err != nil {
			respondInternalError(w, r, err)
			return
		}
		backlogStatusSet = make(map[int]bool)
	case err != nil:
		respondInternalError(w, r, err)
		return
	default:
		// Parse card_fields
		if cardFieldsJSON.Valid && cardFieldsJSON.String != "" {
			_ = json.Unmarshal([]byte(cardFieldsJSON.String), &cardFields)
			cardFields = filterPublicBoardCardFields(cardFields)
		}

		// Parse backlog status IDs (items in these statuses won't appear on the board)
		var backlogStatusIDs []int
		if backlogStatusIDsJSON.Valid && backlogStatusIDsJSON.String != "" {
			_ = json.Unmarshal([]byte(backlogStatusIDsJSON.String), &backlogStatusIDs)
		}
		backlogStatusSet = make(map[int]bool, len(backlogStatusIDs))
		for _, id := range backlogStatusIDs {
			backlogStatusSet[id] = true
		}

		// Load columns with status mappings
		columns, err = h.loadColumnsWithStatuses(boardConfigID)
		if err != nil {
			respondInternalError(w, r, err)
			return
		}
	}

	// Build status-to-column mapping
	statusToColumn := make(map[int]int) // statusID -> column index
	for i, col := range columns {
		for _, sid := range col.statusIDs {
			statusToColumn[sid] = i
		}
	}

	// Resolve the mandatory query scope and narrow it further to the
	// collection's home workspace when one is set.
	scopedWorkspaceIDs, err := h.resolveCollectionWorkspaceIDs(collectionID)
	if err != nil {
		if isPublicBoardScopeError(err) {
			respondNotFound(w, r, "board")
			return
		}
		respondInternalError(w, r, err)
		return
	}
	if len(scopedWorkspaceIDs) == 0 {
		// No workspaces, return empty board
		resp := h.buildEmptyResponse(collectionName, collectionDescription, columns, cardFields, updatedAt, effectiveGroupBy)
		respondJSONOK(w, resp)
		return
	}

	// Execute the collection's QL query to get items
	crudService := services.NewItemCRUDService(h.db)
	items, totalItems, err := crudService.ListWithQL(services.ListWithQLParams{
		CollectionID: collectionID,
		WorkspaceIDs: scopedWorkspaceIDs,
		Pagination:   services.PaginationParams{Limit: publicBoardItemLimit},
		SortBy:       "created_at",
		SortAsc:      false,
	})
	if err != nil {
		respondInternalError(w, r, err)
		return
	}

	// Load labels for all items
	itemLabels := h.loadItemLabels(items)

	// Public milestone lanes are opt-in. Only load and expose milestone data
	// when the owner enabled them, so flat public boards keep the minimal
	// projection.
	var publicMilestones []publicMilestone
	itemMilestoneIDs := make(map[int][]int)
	if effectiveGroupBy == "milestone" {
		if err := repository.NewMilestoneAttachRepository(h.db).LoadForItems(items); err != nil {
			respondInternalError(w, r, err)
			return
		}
		byID := make(map[int]publicMilestone)
		for _, item := range items {
			if len(item.Milestones) == 0 {
				continue
			}
			ids := make([]int, 0, len(item.Milestones))
			for _, milestone := range item.Milestones {
				ids = append(ids, milestone.ID)
				pm := publicMilestone{
					ID:            milestone.ID,
					Name:          milestone.Name,
					Status:        milestone.Status,
					CategoryColor: milestone.CategoryColor,
				}
				if milestone.TargetDate != nil {
					pm.TargetDate = *milestone.TargetDate
				}
				byID[milestone.ID] = pm
			}
			itemMilestoneIDs[item.ID] = ids
		}
		publicMilestones = make([]publicMilestone, 0, len(byID))
		for _, milestone := range byID {
			publicMilestones = append(publicMilestones, milestone)
		}
		sort.Slice(publicMilestones, func(i, j int) bool {
			a, b := publicMilestones[i], publicMilestones[j]
			if (a.TargetDate == "") != (b.TargetDate == "") {
				return b.TargetDate == ""
			}
			if a.TargetDate != b.TargetDate {
				return a.TargetDate < b.TargetDate
			}
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.ID < b.ID
		})
	}

	// Build response columns with grouped items
	responseColumns := make([]publicColumn, len(columns))
	for i, col := range columns {
		responseColumns[i] = publicColumn{
			Name:     col.name,
			Color:    col.color,
			WIPLimit: col.wipLimit,
			Items:    []publicBoardCard{},
		}
	}

	// Project grouping follows inheritance: an item with inherit_project set
	// sits in its parent's project lane, resolved over the loaded items.
	effectiveProjects := resolvePublicEffectiveProjects(items)

	// Customer lanes resolve each effective project to its customer org.
	var projectCustomers map[int]publicProjectCustomer
	if effectiveGroupBy == "customer" {
		projectCustomers, err = h.loadProjectCustomers(effectiveProjects)
		if err != nil {
			respondInternalError(w, r, err)
			return
		}
	}

	// Group items by column
	for _, item := range items {
		if item.StatusID == nil {
			continue
		}
		// Skip backlog items
		if backlogStatusSet[*item.StatusID] {
			continue
		}
		colIdx, found := statusToColumn[*item.StatusID]
		if !found {
			continue
		}

		card := publicBoardCard{
			Key:            item.WorkspaceKey + "-" + itoa(item.WorkspaceItemNumber),
			Title:          item.Title,
			PriorityName:   item.PriorityName,
			PriorityIcon:   item.PriorityIcon,
			PriorityColor:  item.PriorityColor,
			AssigneeName:   item.AssigneeName,
			AssigneeAvatar: item.AssigneeAvatar,
			StatusID:       item.StatusID,
			StatusName:     item.StatusName,
			ItemTypeName:   item.ItemTypeName,
			StoryPoints:    item.StoryPoints,
			MilestoneIDs:   itemMilestoneIDs[item.ID],
		}

		if effective := effectiveProjects[item.ID]; effective != nil {
			id := effective.id
			card.ProjectID = &id
			card.ProjectName = effective.name
		} else if item.ProjectID != nil && !item.InheritProject {
			card.ProjectID = item.ProjectID
			card.ProjectName = item.ProjectName
		}
		card.LaneKey = publicLaneKeyForCard(effectiveGroupBy, &item, effectiveProjects[item.ID])
		if effectiveGroupBy == "customer" {
			if effective := effectiveProjects[item.ID]; effective != nil {
				if customer, ok := projectCustomers[effective.id]; ok && !customer.noCustomer {
					card.customerID = &customer.id
					card.customerName = customer.name
				}
			}
		}

		if item.DueDate != nil {
			card.DueDate = item.DueDate.Format("2006-01-02")
		}

		// Attach labels
		if labels, ok := itemLabels[item.ID]; ok {
			card.Labels = labels
		}

		responseColumns[colIdx].Items = append(responseColumns[colIdx].Items, card)
	}

	var groupLanes []publicBoardLane
	if effectiveGroupBy == "customer" {
		groupLanes = buildCustomerBoardLanes(responseColumns)
	} else {
		groupLanes = buildPublicBoardLanes(effectiveGroupBy, columns, responseColumns)
	}

	resp := publicBoardResponse{
		Columns:     responseColumns,
		CardFields:  cardFields,
		Swimlanes:   effectiveGroupBy == "milestone",
		Milestones:  publicMilestones,
		GroupBy:     effectiveGroupBy,
		Lanes:       groupLanes,
		TotalItems:  totalItems,
		LoadedItems: len(items),
		Truncated:   totalItems > len(items),
		ItemLimit:   publicBoardItemLimit,
		UpdatedAt:   updatedAt,
	}
	resp.Collection.Name = collectionName
	resp.Collection.Description = collectionDescription

	respondJSONOK(w, resp)
}

// publicEffectiveProject is the resolved direct project for one item.
type publicEffectiveProject struct {
	id   int
	name string
}

// resolvePublicEffectiveProjects walks inherit_project up the loaded parent
// chain so grouped lanes match what the item detail shows. The parent may be
// outside the loaded page; an unresolvable inherit falls back to no project.
func resolvePublicEffectiveProjects(items []models.Item) map[int]*publicEffectiveProject {
	byID := make(map[int]*models.Item, len(items))
	for i := range items {
		byID[items[i].ID] = &items[i]
	}

	result := make(map[int]*publicEffectiveProject, len(items))
	visiting := make(map[int]bool, len(items))
	var resolve func(item *models.Item) *publicEffectiveProject
	resolve = func(item *models.Item) *publicEffectiveProject {
		if cached, ok := result[item.ID]; ok {
			return cached
		}
		if visiting[item.ID] {
			return nil
		}
		visiting[item.ID] = true
		defer delete(visiting, item.ID)

		var out *publicEffectiveProject
		switch {
		case item.ProjectID != nil && !item.InheritProject:
			out = &publicEffectiveProject{id: *item.ProjectID, name: item.ProjectName}
		case !item.InheritProject:
			out = nil
		case item.ParentID != nil && *item.ParentID != item.ID:
			if parent, ok := byID[*item.ParentID]; ok {
				out = resolve(parent)
			}
		}
		result[item.ID] = out
		return out
	}
	for i := range items {
		resolve(&items[i])
	}
	return result
}

// publicLaneKeyForCard derives the card's swimlane key for grouped boards.
// Flat and milestone boards leave it empty; milestone grouping keeps its own
// milestone_ids-based derivation in the frontend. Customer grouping buckets
// cards by their project — the customer container is derived in the lane
// builder — so its keys match the project dimension.
func publicLaneKeyForCard(groupBy string, item *models.Item, effective *publicEffectiveProject) string {
	switch groupBy {
	case "assignee":
		if item.AssigneeID == nil {
			return "none"
		}
		return "assignee-" + itoa(*item.AssigneeID)
	case "status":
		if item.StatusID == nil {
			return "none"
		}
		return "status-" + itoa(*item.StatusID)
	case "project", "customer":
		if effective == nil {
			return "none"
		}
		return "project-" + itoa(effective.id)
	default:
		return ""
	}
}

// publicProjectCustomer is the customer organisation behind one project. The
// built-in no-customer placeholder org counts as "no customer".
type publicProjectCustomer struct {
	id         int
	name       string
	noCustomer bool
}

// loadProjectCustomers resolves every effective project on the board to its
// customer organisation with two IN-queries.
func (h *PublicBoardHandler) loadProjectCustomers(effective map[int]*publicEffectiveProject) (map[int]publicProjectCustomer, error) {
	seen := make(map[int]bool, len(effective))
	projectIDs := make([]int, 0, len(effective))
	for _, project := range effective {
		if project == nil || seen[project.id] {
			continue
		}
		seen[project.id] = true
		projectIDs = append(projectIDs, project.id)
	}
	result := make(map[int]publicProjectCustomer, len(projectIDs))
	if len(projectIDs) == 0 {
		return result, nil
	}

	projectArgs := make([]any, len(projectIDs))
	for i, id := range projectIDs {
		projectArgs[i] = id
	}
	projectPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(projectIDs)), ",")

	rows, err := h.db.Query(
		"SELECT id, customer_id FROM time_projects WHERE id IN ("+projectPlaceholders+")",
		projectArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("load project customers: %w", err)
	}
	projectToCustomer := make(map[int]int, len(projectIDs))
	for rows.Next() {
		var projectID int
		var customerID sql.NullInt64
		if err := rows.Scan(&projectID, &customerID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan project customer: %w", err)
		}
		if customerID.Valid {
			projectToCustomer[projectID] = int(customerID.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	seenCustomers := make(map[int]bool, len(projectToCustomer))
	customerIDs := make([]int, 0, len(projectToCustomer))
	for _, customerID := range projectToCustomer {
		if seenCustomers[customerID] {
			continue
		}
		seenCustomers[customerID] = true
		customerIDs = append(customerIDs, customerID)
	}
	if len(customerIDs) == 0 {
		return result, nil
	}

	customerArgs := make([]any, len(customerIDs))
	for i, id := range customerIDs {
		customerArgs[i] = id
	}
	customerPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(customerIDs)), ",")
	crows, err := h.db.Query(
		"SELECT id, name, COALESCE(builtin_key, '') FROM customer_organisations WHERE id IN ("+customerPlaceholders+")",
		customerArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("load customers: %w", err)
	}
	customers := make(map[int]publicProjectCustomer, len(customerIDs))
	for crows.Next() {
		var id int
		var name, builtinKey string
		if err := crows.Scan(&id, &name, &builtinKey); err != nil {
			_ = crows.Close()
			return nil, fmt.Errorf("scan customer: %w", err)
		}
		customers[id] = publicProjectCustomer{
			id:         id,
			name:       name,
			noCustomer: builtinKey == "no_customer",
		}
	}
	if err := crows.Err(); err != nil {
		_ = crows.Close()
		return nil, err
	}
	_ = crows.Close()

	for projectID, customerID := range projectToCustomer {
		if customer, ok := customers[customerID]; ok {
			result[projectID] = customer
		}
	}
	return result, nil
}

// buildCustomerBoardLanes nests project lanes under their customer: one
// container lane per customer with a child lane per distinct project, both
// ordered by name; the no-customer container and the projectless lane sort
// last. Containers without cards cannot occur — they are derived from cards.
func buildCustomerBoardLanes(responseColumns []publicColumn) []publicBoardLane {
	type childLane struct {
		key   string
		title string
	}
	type container struct {
		key      string
		title    string
		children []childLane
		seen     map[string]bool
	}

	containers := make(map[string]*container)
	add := func(customerKey, customerTitle, laneKey, laneTitle string) {
		bucket := containers[customerKey]
		if bucket == nil {
			bucket = &container{key: customerKey, title: customerTitle, seen: make(map[string]bool)}
			containers[customerKey] = bucket
		}
		if bucket.seen[laneKey] {
			return
		}
		bucket.seen[laneKey] = true
		bucket.children = append(bucket.children, childLane{key: laneKey, title: laneTitle})
	}

	for _, column := range responseColumns {
		for _, card := range column.Items {
			laneKey := card.LaneKey
			if laneKey == "" {
				laneKey = "none"
			}
			laneTitle := card.ProjectName
			if laneKey == "none" {
				laneTitle = "No project"
			} else if laneTitle == "" {
				laneTitle = "Project #" + strings.TrimPrefix(laneKey, "project-")
			}

			customerKey, customerTitle := "customer-none", "No customer"
			if card.customerID != nil {
				customerKey = "customer-" + itoa(*card.customerID)
				customerTitle = card.customerName
				if customerTitle == "" {
					customerTitle = "Customer #" + itoa(*card.customerID)
				}
			}
			add(customerKey, customerTitle, laneKey, laneTitle)
		}
	}

	ordered := make([]*container, 0, len(containers))
	for _, bucket := range containers {
		ordered = append(ordered, bucket)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].key == "customer-none" {
			return false
		}
		if ordered[j].key == "customer-none" {
			return true
		}
		if ordered[i].title != ordered[j].title {
			return ordered[i].title < ordered[j].title
		}
		return ordered[i].key < ordered[j].key
	})

	lanes := make([]publicBoardLane, 0, len(ordered))
	for _, bucket := range ordered {
		children := bucket.children
		sort.Slice(children, func(i, j int) bool {
			if children[i].key == "none" {
				return false
			}
			if children[j].key == "none" {
				return true
			}
			if children[i].title != children[j].title {
				return children[i].title < children[j].title
			}
			return children[i].key < children[j].key
		})
		childLanes := make([]publicBoardLane, 0, len(children))
		for _, entry := range children {
			childLanes = append(childLanes, publicBoardLane{Key: entry.key, Title: entry.title})
		}
		lanes = append(lanes, publicBoardLane{
			Key:      bucket.key,
			Title:    bucket.title,
			Children: childLanes,
		})
	}
	return lanes
}

// buildPublicBoardLanes builds the ordered lane headers for a grouped public
// board. Only lanes that contain at least one card are emitted; a trailing
// "none" lane collects cards without a value.
func buildPublicBoardLanes(groupBy string, columns []boardColumnInfo, responseColumns []publicColumn) []publicBoardLane {
	countCards := func(predicate func(publicBoardCard) bool) int {
		total := 0
		for _, col := range responseColumns {
			for _, card := range col.Items {
				if predicate(card) {
					total++
				}
			}
		}
		return total
	}

	switch groupBy {
	case "assignee", "project":
		titles := make(map[string]string)
		for _, col := range responseColumns {
			for _, card := range col.Items {
				key := card.LaneKey
				if key == "" || key == "none" {
					continue
				}
				if _, ok := titles[key]; !ok {
					switch {
					case groupBy == "assignee":
						if card.AssigneeName != "" {
							titles[key] = card.AssigneeName
						} else {
							titles[key] = "Unassigned"
						}
					case card.ProjectName != "":
						titles[key] = card.ProjectName
					default:
						titles[key] = "Project #" + strings.TrimPrefix(key, "project-")
					}
				}
			}
		}
		lanes := make([]publicBoardLane, 0, len(titles)+1)
		for key, title := range titles {
			lanes = append(lanes, publicBoardLane{Key: key, Title: title})
		}
		sort.Slice(lanes, func(i, j int) bool {
			if lanes[i].Title != lanes[j].Title {
				return lanes[i].Title < lanes[j].Title
			}
			return lanes[i].Key < lanes[j].Key
		})
		if countCards(func(card publicBoardCard) bool {
			return card.LaneKey == "" || card.LaneKey == "none"
		}) > 0 {
			lanes = append(lanes, publicBoardLane{Key: "none", Title: "Unassigned"})
		}
		return lanes
	case "status":
		titlesByKey := make(map[string]string)
		for _, col := range responseColumns {
			for _, card := range col.Items {
				if card.LaneKey == "" || card.LaneKey == "none" {
					continue
				}
				if card.StatusName != "" {
					titlesByKey[card.LaneKey] = card.StatusName
				}
			}
		}
		lanes := make([]publicBoardLane, 0, len(titlesByKey))
		emitted := make(map[string]bool, len(titlesByKey))
		// Board column order is the canonical status order for this view.
		for _, col := range columns {
			for _, statusID := range col.statusIDs {
				key := "status-" + itoa(statusID)
				title, ok := titlesByKey[key]
				if !ok || emitted[key] {
					continue
				}
				emitted[key] = true
				lanes = append(lanes, publicBoardLane{Key: key, Title: title})
			}
		}
		// Statuses outside every column cannot occur, but never drop a card.
		var leftover []publicBoardLane
		for key, title := range titlesByKey {
			if emitted[key] {
				continue
			}
			leftover = append(leftover, publicBoardLane{Key: key, Title: title})
		}
		sort.Slice(leftover, func(i, j int) bool {
			if leftover[i].Title != leftover[j].Title {
				return leftover[i].Title < leftover[j].Title
			}
			return leftover[i].Key < leftover[j].Key
		})
		return append(lanes, leftover...)
	default:
		return nil
	}
}

// boardColumnInfo holds column data loaded from the DB
type boardColumnInfo struct {
	name      string
	color     string
	wipLimit  *int
	statusIDs []int
}

func (h *PublicBoardHandler) loadColumnsWithStatuses(configID int) ([]boardColumnInfo, error) {
	rows, err := h.db.Query(`
		SELECT id, name, wip_limit, color
		FROM board_columns
		WHERE board_configuration_id = ?
		ORDER BY display_order
	`, configID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type colRow struct {
		id       int
		name     string
		wipLimit *int
		color    string
	}
	var colRows []colRow
	for rows.Next() {
		var c colRow
		var wipLimit sql.NullInt64
		if err := rows.Scan(&c.id, &c.name, &wipLimit, &c.color); err != nil {
			return nil, err
		}
		if wipLimit.Valid {
			v := int(wipLimit.Int64)
			c.wipLimit = &v
		}
		colRows = append(colRows, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	columns := make([]boardColumnInfo, len(colRows))
	for i, cr := range colRows {
		columns[i] = boardColumnInfo{
			name:     cr.name,
			color:    cr.color,
			wipLimit: cr.wipLimit,
		}

		// Load status mappings
		srows, err := h.db.Query(`SELECT status_id FROM board_column_statuses WHERE board_column_id = ?`, cr.id)
		if err != nil {
			return nil, err
		}
		var statusIDs []int
		for srows.Next() {
			var sid int
			if err := srows.Scan(&sid); err != nil {
				_ = srows.Close()
				return nil, err
			}
			statusIDs = append(statusIDs, sid)
		}
		if err := srows.Err(); err != nil {
			_ = srows.Close()
			return nil, err
		}
		_ = srows.Close()
		columns[i].statusIDs = statusIDs
	}

	return columns, nil
}

func (h *PublicBoardHandler) buildDefaultColumns() ([]boardColumnInfo, error) {
	// Load status categories ordered: non-completed first, then completed
	catRows, err := h.db.Query(`SELECT id, name, color FROM status_categories ORDER BY is_completed ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = catRows.Close() }()

	type category struct {
		id    int
		name  string
		color string
	}
	var categories []category
	for catRows.Next() {
		var c category
		if err := catRows.Scan(&c.id, &c.name, &c.color); err != nil {
			return nil, err
		}
		categories = append(categories, c)
	}
	if err := catRows.Err(); err != nil {
		return nil, err
	}

	// Load all statuses grouped by category
	statusRows, err := h.db.Query(`SELECT id, category_id FROM statuses ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = statusRows.Close() }()

	statusesByCat := make(map[int][]int)
	for statusRows.Next() {
		var sid, catID int
		if err := statusRows.Scan(&sid, &catID); err != nil {
			return nil, err
		}
		statusesByCat[catID] = append(statusesByCat[catID], sid)
	}
	if err := statusRows.Err(); err != nil {
		return nil, err
	}

	// Build columns: one per category
	columns := make([]boardColumnInfo, 0, len(categories))
	for _, cat := range categories {
		columns = append(columns, boardColumnInfo{
			name:      cat.name,
			color:     cat.color,
			statusIDs: statusesByCat[cat.id],
		})
	}
	return columns, nil
}

// resolveCollectionWorkspaceIDs derives the anonymous read boundary from the
// collection query and intersects it with the optional home workspace.
func (h *PublicBoardHandler) resolveCollectionWorkspaceIDs(collectionID int) ([]int, error) {
	var wsID sql.NullInt64
	var qlQuery string
	err := h.db.QueryRow(`SELECT workspace_id, COALESCE(ql_query, '') FROM collections WHERE id = ?`, collectionID).Scan(&wsID, &qlQuery)
	if err != nil {
		return nil, err
	}
	workspaceIDs, err := h.publicBoardScope.ResolveWorkspaceIDs(qlQuery)
	if err != nil {
		return nil, err
	}
	if !wsID.Valid {
		return workspaceIDs, nil
	}
	for _, workspaceID := range workspaceIDs {
		if workspaceID == int(wsID.Int64) {
			return []int{workspaceID}, nil
		}
	}
	return []int{}, nil
}

func (h *PublicBoardHandler) loadItemLabels(items []models.Item) map[int][]publicLabel {
	if len(items) == 0 {
		return nil
	}

	result := make(map[int][]publicLabel)
	for _, item := range items {
		if len(item.Labels) > 0 {
			var labels []publicLabel
			for _, l := range item.Labels {
				labels = append(labels, publicLabel{Name: l.Name, Color: l.Color})
			}
			result[item.ID] = labels
		}
	}
	return result
}

func (h *PublicBoardHandler) buildEmptyResponse(name, desc string, columns []boardColumnInfo, cardFields []models.ListColumn, updatedAt, groupBy string) publicBoardResponse {
	resp := publicBoardResponse{
		CardFields: cardFields,
		Swimlanes:  groupBy == "milestone",
		GroupBy:    groupBy,
		ItemLimit:  publicBoardItemLimit,
		UpdatedAt:  updatedAt,
	}
	resp.Collection.Name = name
	resp.Collection.Description = desc
	for _, col := range columns {
		resp.Columns = append(resp.Columns, publicColumn{
			Name:     col.name,
			Color:    col.color,
			WIPLimit: col.wipLimit,
			Items:    []publicBoardCard{},
		})
	}
	return resp
}

func (h *PublicBoardHandler) loadSingleItemLabels(itemID int) []publicLabel {
	rows, err := h.db.Query(`
		SELECT l.name, l.color
		FROM labels l
		JOIN item_labels il ON il.label_id = l.id
		WHERE il.item_id = ?
	`, itemID)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var labels []publicLabel
	for rows.Next() {
		var l publicLabel
		if err := rows.Scan(&l.Name, &l.Color); err != nil {
			return labels
		}
		labels = append(labels, l)
	}
	if err := rows.Err(); err != nil {
		return labels
	}
	return labels
}

func (h *PublicBoardHandler) loadPublicComments(itemID int, slug string) ([]publicComment, error) {
	rows, err := h.db.Query(`
		SELECT COALESCE(u.first_name || ' ' || u.last_name, pc.name, 'Unknown'),
		       COALESCE(u.avatar_url, ''),
		       c.content, c.created_at
		FROM comments c
		LEFT JOIN users u ON c.author_id = u.id
		LEFT JOIN portal_customers pc ON c.portal_customer_id = pc.id
		WHERE c.item_id = ? AND c.is_private = false
		ORDER BY c.created_at ASC
	`, itemID)
	if err != nil {
		return nil, fmt.Errorf("failed to load comments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var comments []publicComment
	for rows.Next() {
		var c publicComment
		if err := rows.Scan(&c.AuthorName, &c.AuthorAvatar, &c.Content, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan comment: %w", err)
		}
		c.Content = rewritePublicAttachmentURLs(c.Content, slug)
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if comments == nil {
		comments = []publicComment{}
	}
	return comments, nil
}

// DownloadAttachment serves an image attachment for a public board item
func (h *PublicBoardHandler) DownloadAttachment(w http.ResponseWriter, r *http.Request) {
	// Publication and membership are mutable authorization inputs; never cache.
	w.Header().Set("Cache-Control", "no-store")

	slug := r.PathValue("slug")
	attachmentIDStr := r.PathValue("id")
	if slug == "" || attachmentIDStr == "" {
		respondNotFound(w, r, "attachment")
		return
	}

	attachmentID, err := strconv.Atoi(attachmentIDStr)
	if err != nil {
		respondNotFound(w, r, "attachment")
		return
	}

	// Validate slug → public collection
	var collectionID int
	err = h.db.QueryRow(`
		SELECT id FROM collections
		WHERE public_slug = ? AND is_public = true AND is_private = false AND public_slug IS NOT NULL
	`, slug).Scan(&collectionID)
	if err != nil {
		respondNotFound(w, r, "attachment")
		return
	}

	// Require an item attachment; legacy rows reuse item_id for other entities.
	var itemID sql.NullInt64
	var entityType sql.NullString
	var filePath, mimeType, originalFilename string
	var fileSize int64
	err = h.db.QueryRow(`
		SELECT item_id, COALESCE(entity_type, 'item'), file_path, mime_type, original_filename, file_size
		FROM attachments WHERE id = ?
	`, attachmentID).Scan(&itemID, &entityType, &filePath, &mimeType, &originalFilename, &fileSize)
	if err != nil {
		respondNotFound(w, r, "attachment")
		return
	}

	if !itemID.Valid || (entityType.String != "" && entityType.String != "item") {
		respondNotFound(w, r, "attachment")
		return
	}

	// SVG is excluded because it can execute scripts.
	if !strings.HasPrefix(mimeType, "image/") || mimeType == "image/svg+xml" {
		respondNotFound(w, r, "attachment")
		return
	}

	belongs, err := h.itemBelongsToCollection(int(itemID.Int64), collectionID)
	if err != nil {
		respondInternalError(w, r, err)
		return
	}
	if !belongs {
		respondNotFound(w, r, "attachment")
		return
	}

	// Read through the configured backend and hide traversal or missing-object
	// failures as 404.
	file, err := h.storage.Open(r.Context(), filePath)
	if err != nil {
		respondNotFound(w, r, "attachment")
		return
	}
	defer func() { _ = file.Close() }()

	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size(), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Disposition", fileserve.ContentDisposition("inline", originalFilename))

	_, _ = io.Copy(w, file)
}

// itemBelongsToCollection checks the collection's workspace-scoped QL result.
func (h *PublicBoardHandler) itemBelongsToCollection(itemID, collectionID int) (bool, error) {
	scopedWorkspaceIDs, err := h.resolveCollectionWorkspaceIDs(collectionID)
	if err != nil {
		if isPublicBoardScopeError(err) {
			return false, nil
		}
		return false, err
	}

	crudService := services.NewItemCRUDService(h.db)
	items, _, err := crudService.ListWithQL(services.ListWithQLParams{
		CollectionID: collectionID,
		WorkspaceIDs: scopedWorkspaceIDs,
		Filters:      services.ItemFilters{ItemID: &itemID},
		Pagination:   services.PaginationParams{Limit: 1},
		SortBy:       "created_at",
		SortAsc:      false,
	})
	if err != nil {
		return false, err
	}

	return len(items) > 0, nil
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
