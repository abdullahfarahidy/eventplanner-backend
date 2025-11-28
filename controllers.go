package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// -----------------------------
// Helper functions
// -----------------------------

func jsonError(c *gin.Context, code int, msg string) {
	c.JSON(code, gin.H{"error": msg})
}

// getUserIDFromContext expects AuthMiddleware to set "user_id" (uint) in context.
// If not present -> unauthorized.
func getUserIDFromContext(c *gin.Context) (uint, bool) {
	uid, exists := c.Get("user_id")
	if !exists {
		return 0, false
	}
	// type assert to uint (the middleware should set uint)
	switch v := uid.(type) {
	case uint:
		return v, true
	case int:
		return uint(v), true
	case float64:
		return uint(v), true
	default:
		_ = v
		return 0, false
	}
}

// -----------------------------
// Events
// -----------------------------

type CreateEventRequest struct {
	Title       string `json:"title" binding:"required"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Date        string `json:"date" binding:"required"` // expect ISO8601 or "YYYY-MM-DD"
}

func CreateEvent(c *gin.Context) {
	userID, ok := getUserIDFromContext(c)
	if !ok {
		jsonError(c, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body CreateEventRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// parse date - accept RFC3339 or YYYY-MM-DD
	var eventDate time.Time
	var err error
	eventDate, err = time.Parse(time.RFC3339, body.Date)
	if err != nil {
		eventDate, err = time.Parse("2006-01-02", body.Date)
		if err != nil {
			jsonError(c, http.StatusBadRequest, "invalid date format (use RFC3339 or YYYY-MM-DD)")
			return
		}
	}

	ev := Event{
		Title:       strings.TrimSpace(body.Title),
		Description: body.Description,
		Location:    body.Location,
		Date:        eventDate,
		OrganizerID: userID,
	}

	if err := DB.Create(&ev).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "could not create event: "+err.Error())
		return
	}

	// Ensure the creator is marked as organizer in attendees table
	org := EventAttendee{
		EventID: ev.ID,
		UserID:  userID,
		Role:    "organizer",
		Status:  "",
	}
	// Try to create but ignore duplicate errors (shouldn't exist)
	_ = DB.Where("event_id = ? AND user_id = ?", ev.ID, userID).FirstOrCreate(&org)

	c.JSON(http.StatusCreated, ev)
}

func GetOrganizedEvents(c *gin.Context) {
	userID, ok := getUserIDFromContext(c)
	if !ok {
		jsonError(c, http.StatusUnauthorized, "unauthorized")
		return
	}

	var events []Event
	if err := DB.Preload("Tasks").Where("organizer_id = ?", userID).Order("date asc").Find(&events).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, events)
}

func GetInvitedEvents(c *gin.Context) {
	userID, ok := getUserIDFromContext(c)
	if !ok {
		jsonError(c, http.StatusUnauthorized, "unauthorized")
		return
	}

	var attendances []EventAttendee
	if err := DB.Where("user_id = ? AND role = ?", userID, "attendee").Find(&attendances).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}

	if len(attendances) == 0 {
		c.JSON(http.StatusOK, []Event{})
		return
	}

	ids := make([]uint, 0, len(attendances))
	for _, a := range attendances {
		ids = append(ids, a.EventID)
	}

	var events []Event
	if err := DB.Preload("Tasks").Where("id IN ?", ids).Order("date asc").Find(&events).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, events)
}

func DeleteEvent(c *gin.Context) {
	userID, ok := getUserIDFromContext(c)
	if !ok {
		jsonError(c, http.StatusUnauthorized, "unauthorized")
		return
	}

	idParam := c.Param("id")
	if idParam == "" {
		jsonError(c, http.StatusBadRequest, "missing event id")
		return
	}
	id, err := strconv.Atoi(idParam)
	if err != nil {
		jsonError(c, http.StatusBadRequest, "invalid event id")
		return
	}

	var ev Event
	if err := DB.First(&ev, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			jsonError(c, http.StatusNotFound, "event not found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}

	// Only organizer can delete
	if ev.OrganizerID != userID {
		jsonError(c, http.StatusForbidden, "only organizer can delete the event")
		return
	}

	// Delete tasks and attendee links and event in a transaction
	if err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("event_id = ?", ev.ID).Delete(&EventAttendee{}).Error; err != nil {
			return err
		}
		if err := tx.Where("event_id = ?", ev.ID).Delete(&Task{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&Event{}, ev.ID).Error; err != nil {
			return err
		}
		return nil
	}); err != nil {
		jsonError(c, http.StatusInternalServerError, "delete failed: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "event deleted"})
}

// -----------------------------
// Invitations
// -----------------------------

type InviteRequest struct {
	UserID uint `json:"user_id" binding:"required"`
	// EventID is taken from URL param :id
}

func InviteUser(c *gin.Context) {
	userID, ok := getUserIDFromContext(c)
	if !ok {
		jsonError(c, http.StatusUnauthorized, "unauthorized")
		return
	}

	// event id from path
	idParam := c.Param("id")
	eventID64, err := strconv.ParseUint(idParam, 10, 64)
	if err != nil {
		jsonError(c, http.StatusBadRequest, "invalid event id")
		return
	}
	eventID := uint(eventID64)

	// bind body
	var body InviteRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}

	// check event exists
	var ev Event
	if err := DB.First(&ev, eventID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			jsonError(c, http.StatusNotFound, "event not found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}

	// Only organizer (creator) or someone already marked as collaborator can invite.
	// Simplest rule: only organizer can invite.
	if ev.OrganizerID != userID {
		jsonError(c, http.StatusForbidden, "only organizer can invite others")
		return
	}

	// check invited user exists
	var invitee User
	if err := DB.First(&invitee, body.UserID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			jsonError(c, http.StatusNotFound, "invited user not found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}

	// prevent inviting organizer as attendee (if same user)
	if invitee.ID == ev.OrganizerID {
		jsonError(c, http.StatusBadRequest, "user is already organizer")
		return
	}

	// Idempotent create: don't duplicate attendee rows
	var existing EventAttendee
	if err := DB.Where("event_id = ? AND user_id = ?", eventID, invitee.ID).First(&existing).Error; err == nil {
		// exists already
		c.JSON(http.StatusOK, gin.H{"message": "user already invited or participant"})
		return
	} else if err != nil && err != gorm.ErrRecordNotFound {
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}

	att := EventAttendee{
		EventID: eventID,
		UserID:  invitee.ID,
		Role:    "attendee",
		Status:  "",
	}
	if err := DB.Create(&att).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "could not create invitation: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "user invited"})
}

// -----------------------------
// Attendance
// -----------------------------

type AttendanceRequest struct {
	Status string `json:"status" binding:"required"` // Going / Maybe / Not Going
	// EventID is in path param /events/:id/respond
}

func SetAttendance(c *gin.Context) {
	userID, ok := getUserIDFromContext(c)
	if !ok {
		jsonError(c, http.StatusUnauthorized, "unauthorized")
		return
	}

	idParam := c.Param("id")
	eventID64, err := strconv.ParseUint(idParam, 10, 64)
	if err != nil {
		jsonError(c, http.StatusBadRequest, "invalid event id")
		return
	}
	eventID := uint(eventID64)

	// validate request body
	var body AttendanceRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	normalized := strings.Title(strings.ToLower(strings.TrimSpace(body.Status)))
	if normalized != "Going" && normalized != "Maybe" && normalized != "Not Going" {
		jsonError(c, http.StatusBadRequest, "status must be one of: Going, Maybe, Not Going")
		return
	}

	// Ensure event exists
	var ev Event
	if err := DB.First(&ev, eventID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			jsonError(c, http.StatusNotFound, "event not found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}

	// If there is no attendee row for this user, create one as attendee (self-rsvp)
	var att EventAttendee
	if err := DB.Where("event_id = ? AND user_id = ?", eventID, userID).First(&att).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			// create attendee record with status
			att = EventAttendee{
				EventID: eventID,
				UserID:  userID,
				Role:    "attendee",
				Status:  normalized,
			}
			if err := DB.Create(&att).Error; err != nil {
				jsonError(c, http.StatusInternalServerError, "could not set attendance: "+err.Error())
				return
			}
			c.JSON(http.StatusOK, att)
			return
		}
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}

	// Update existing record
	att.Status = normalized
	if err := DB.Save(&att).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "could not update status: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, att)
}

func GetEventAttendees(c *gin.Context) {
	userID, ok := getUserIDFromContext(c)
	if !ok {
		jsonError(c, http.StatusUnauthorized, "unauthorized")
		return
	}

	idParam := c.Param("id")
	eventID64, err := strconv.ParseUint(idParam, 10, 64)
	if err != nil {
		jsonError(c, http.StatusBadRequest, "invalid event id")
		return
	}
	eventID := uint(eventID64)

	// Only organizer can view full attendee list
	var ev Event
	if err := DB.First(&ev, eventID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			jsonError(c, http.StatusNotFound, "event not found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}

	if ev.OrganizerID != userID {
		jsonError(c, http.StatusForbidden, "only organizer can view attendees")
		return
	}

	var attendees []EventAttendee
	if err := DB.Where("event_id = ?", eventID).Find(&attendees).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, attendees)
}

// -----------------------------
// Tasks
// -----------------------------

type CreateTaskRequest struct {
	Title       string `json:"title" binding:"required"`
	Description string `json:"description"`
	// EventID will come from url param :id
}

func CreateTask(c *gin.Context) {
	userID, ok := getUserIDFromContext(c)
	if !ok {
		jsonError(c, http.StatusUnauthorized, "unauthorized")
		return
	}

	// get event id from URL
	idParam := c.Param("id")
	eventID64, err := strconv.ParseUint(idParam, 10, 64)
	if err != nil {
		jsonError(c, http.StatusBadRequest, "invalid event id")
		return
	}
	eventID := uint(eventID64)

	// check event exists and user is allowed to create tasks (organizer only)
	var ev Event
	if err := DB.First(&ev, eventID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			jsonError(c, http.StatusNotFound, "event not found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}
	if ev.OrganizerID != userID {
		jsonError(c, http.StatusForbidden, "only organizer can create tasks")
		return
	}

	var body CreateTaskRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}

	task := Task{
		EventID:     eventID,
		Title:       strings.TrimSpace(body.Title),
		Description: body.Description,
	}

	if err := DB.Create(&task).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "could not create task: "+err.Error())
		return
	}

	c.JSON(http.StatusCreated, task)
}

func GetTasksByEvent(c *gin.Context) {
	// returns tasks for a given event (any authenticated user)
	idParam := c.Param("id")
	eventID64, err := strconv.ParseUint(idParam, 10, 64)
	if err != nil {
		jsonError(c, http.StatusBadRequest, "invalid event id")
		return
	}
	eventID := uint(eventID64)

	var tasks []Task
	if err := DB.Where("event_id = ?", eventID).Find(&tasks).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, tasks)
}

// -----------------------------
// Search (events and tasks)
// -----------------------------
//
// GET /api/events/search?keyword=&start_date=&end_date=&role=organizer|attendee&type=event|task|both
//
// - keyword searches event.title, event.description, task.title, task.description (depending on type)
// - date filters event.date
// - role filters results where user is organizer or attendee (based on the authenticated user)
// - returns [] of { type: "event"/"task", event: {...} } or { type: "task", task: {...}, event: {...} }
//
type SearchRequest struct {
	Keyword   string `form:"keyword" json:"keyword"`
	StartDate string `form:"start_date" json:"start_date"`
	EndDate   string `form:"end_date" json:"end_date"`
	Role      string `form:"role" json:"role"`
	Type      string `form:"type" json:"type"`
}

func SearchHandler(c *gin.Context) {
	userID, ok := getUserIDFromContext(c)
	if !ok {
		jsonError(c, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req SearchRequest
	// support query params (GET) and JSON (POST)
	if c.Request.Method == http.MethodGet {
		if err := c.ShouldBindQuery(&req); err != nil {
			// ignore, we'll validate later
			_ = err
		}
	} else {
		if err := c.ShouldBindJSON(&req); err != nil {
			_ = err
			_ = c.ShouldBindQuery(&req)
		}
	}

	// default type
	if req.Type == "" {
		req.Type = "both"
	}

	// parse dates (accept RFC3339 or YYYY-MM-DD)
	var start, end time.Time
	var err error
	if req.StartDate != "" {
		start, err = time.Parse(time.RFC3339, req.StartDate)
		if err != nil {
			start, err = time.Parse("2006-01-02", req.StartDate)
			if err != nil {
				jsonError(c, http.StatusBadRequest, "invalid start_date format")
				return
			}
		}
	}
	if req.EndDate != "" {
		end, err = time.Parse(time.RFC3339, req.EndDate)
		if err != nil {
			end, err = time.Parse("2006-01-02", req.EndDate)
			if err != nil {
				jsonError(c, http.StatusBadRequest, "invalid end_date format")
				return
			}
		}
		// include whole day
		end = end.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
	}

	keyword := strings.TrimSpace(req.Keyword)
	kw := "%" + keyword + "%"

	results := make([]interface{}, 0)

	// Helper: check role filtering condition for events/tasks
	// If req.Role == "organizer" -> only events where OrganizerID = userID
	// If req.Role == "attendee" -> only events where user is attendee
	// If empty -> no role filtering

	// Search events
	if req.Type == "both" || req.Type == "event" {
		query := DB.Model(&Event{}).Preload("Tasks")

		if keyword != "" {
			query = query.Where("title ILIKE ? OR description ILIKE ?", kw, kw)
		}
		if !start.IsZero() {
			query = query.Where("date >= ?", start)
		}
		if !end.IsZero() {
			query = query.Where("date <= ?", end)
		}

		if req.Role != "" {
			if req.Role == "organizer" {
				query = query.Where("organizer_id = ?", userID)
			} else if req.Role == "attendee" {
				// join with attendees table
				query = query.Joins("JOIN event_attendees ea ON ea.event_id = events.id").
					Where("ea.user_id = ? AND ea.role = ?", userID, "attendee")
			} else {
				jsonError(c, http.StatusBadRequest, "role must be 'organizer' or 'attendee'")
				return
			}
		}

		var events []Event
		if err := query.Order("date asc").Find(&events).Error; err != nil {
			jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
			return
		}
		for _, e := range events {
			results = append(results, gin.H{"type": "event", "event": e})
		}
	}

	// Search tasks (and attach event info)
	if req.Type == "both" || req.Type == "task" {
		// We'll find tasks joining with events to apply date filters and role constraints
		taskQuery := DB.Model(&Task{}).Joins("JOIN events ON events.id = tasks.event_id")

		if keyword != "" {
			// search task title/description or parent event title/description
			taskQuery = taskQuery.Where("tasks.title ILIKE ? OR tasks.description ILIKE ? OR events.title ILIKE ? OR events.description ILIKE ?", kw, kw, kw, kw)
		}
		if !start.IsZero() {
			taskQuery = taskQuery.Where("events.date >= ?", start)
		}
		if !end.IsZero() {
			taskQuery = taskQuery.Where("events.date <= ?", end)
		}
		if req.Role != "" {
			if req.Role == "organizer" {
				taskQuery = taskQuery.Where("events.organizer_id = ?", userID)
			} else if req.Role == "attendee" {
				// ensure user is attendee in event_attendees
				taskQuery = taskQuery.Joins("JOIN event_attendees ea ON ea.event_id = events.id").
					Where("ea.user_id = ? AND ea.role = ?", userID, "attendee")
			} else {
				jsonError(c, http.StatusBadRequest, "role must be 'organizer' or 'attendee'")
				return
			}
		}

		// fetch matching tasks
		var tasks []Task
		if err := taskQuery.Select("tasks.*").Order("events.date asc").Find(&tasks).Error; err != nil {
			jsonError(c, http.StatusInternalServerError, "db error: "+err.Error())
			return
		}

		// attach event data for each task
		for _, t := range tasks {
			var ev Event
			if err := DB.Where("id = ?", t.EventID).First(&ev).Error; err != nil {
				// skip if cannot find parent event
				continue
			}
			results = append(results, gin.H{"type": "task", "task": t, "event": ev})
		}
	}

	c.JSON(http.StatusOK, results)
}
