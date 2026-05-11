package schedule

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Schedule defines a recurring live stream event.
type Schedule struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Days        []string `json:"days"`        // "sunday", "monday", etc.
	Time        string   `json:"time"`        // "08:45" (24-hour)
	Timezone    string   `json:"timezone"`    // "America/Chicago"
	DurationMin int      `json:"durationMin"` // default 120
	PresetID    string   `json:"presetId"`
	Title       string   `json:"title"`       // YouTube broadcast title
	Description string   `json:"description"` // YouTube broadcast description
	Privacy     string   `json:"privacy"`     // "public", "unlisted", "private"
	Enabled     bool     `json:"enabled"`
}

// Override defines a one-time special event.
//
// Client may send either:
//   - StartTime (RFC3339 UTC, server uses as-is), or
//   - WallClock ("2026-04-12T08:45") + Timezone ("America/Chicago"),
//     which the server interprets in that timezone. This is needed because
//     HTML datetime-local inputs return a wall-clock string without zone info.
type Override struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	StartTime   time.Time `json:"startTime,omitempty"`
	WallClock   string    `json:"wallClock,omitempty"`
	Timezone    string    `json:"timezone,omitempty"`
	DurationMin int       `json:"durationMin"`
	PresetID    string    `json:"presetId"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Privacy     string    `json:"privacy"`
}

// Event is a computed upcoming event from a schedule or override.
type Event struct {
	ScheduleID  string    `json:"scheduleId,omitempty"`
	OverrideID  string    `json:"overrideId,omitempty"`
	Name        string    `json:"name"`
	StartTime   time.Time `json:"startTime"`
	DurationMin int       `json:"durationMin"`
	PresetID    string    `json:"presetId"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Privacy     string    `json:"privacy"`
	BroadcastID string    `json:"broadcastId,omitempty"`
	StreamID    string    `json:"streamId,omitempty"`
}

// storeData is the JSON file format.
type storeData struct {
	Schedules  []Schedule           `json:"schedules"`
	Overrides  []Override           `json:"overrides"`
	Broadcasts map[string]string    `json:"broadcasts"`        // eventKey -> YouTube broadcast ID
	Streams    map[string]string    `json:"streams"`           // eventKey -> YouTube stream ID
	Skipped    map[string]time.Time `json:"skipped,omitempty"` // eventKey -> suppressed until
}

// Store manages schedules and overrides, persisted to a JSON file.
type Store struct {
	mu   sync.Mutex
	file string
	data storeData
}

// NewStore loads or creates a schedule store at the given path.
func NewStore(file string) (*Store, error) {
	s := &Store{file: file}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(file)
	if err == nil {
		_ = json.Unmarshal(raw, &s.data)
	}
	if s.data.Broadcasts == nil {
		s.data.Broadcasts = make(map[string]string)
	}
	if s.data.Streams == nil {
		s.data.Streams = make(map[string]string)
	}
	if s.data.Skipped == nil {
		s.data.Skipped = make(map[string]time.Time)
	}
	return s, nil
}

// Schedules returns all recurring schedules.
func (s *Store) Schedules() []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Schedule, len(s.data.Schedules))
	copy(out, s.data.Schedules)
	return out
}

// CreateSchedule adds a new recurring schedule.
func (s *Store) CreateSchedule(sched Schedule) (Schedule, error) {
	sched, err := normalizeSchedule(sched)
	if err != nil {
		return Schedule{}, err
	}
	sched.ID = newID()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Schedules = append(s.data.Schedules, sched)
	return sched, s.save()
}

func normalizeSchedule(sched Schedule) (Schedule, error) {
	if sched.Name == "" {
		return Schedule{}, fmt.Errorf("name is required")
	}
	if len(sched.Days) == 0 {
		return Schedule{}, fmt.Errorf("at least one day is required")
	}
	if sched.Time == "" {
		return Schedule{}, fmt.Errorf("time is required")
	}
	if sched.Timezone == "" {
		sched.Timezone = "America/Chicago"
	}
	if _, err := time.LoadLocation(sched.Timezone); err != nil {
		return Schedule{}, fmt.Errorf("invalid timezone: %s", sched.Timezone)
	}
	if sched.DurationMin <= 0 {
		sched.DurationMin = 120
	}
	if sched.Privacy == "" {
		sched.Privacy = "unlisted"
	}
	return sched, nil
}

// UpdateSchedule replaces a schedule by ID.
func (s *Store) UpdateSchedule(sched Schedule) (Schedule, error) {
	sched, err := normalizeSchedule(sched)
	if err != nil {
		return Schedule{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.data.Schedules {
		if existing.ID == sched.ID {
			sched.ID = existing.ID
			s.data.Schedules[i] = sched
			s.clearBroadcastsForIDLocked(sched.ID)
			return sched, s.save()
		}
	}
	return Schedule{}, fmt.Errorf("schedule %s not found", sched.ID)
}

// DeleteSchedule removes a schedule by ID.
func (s *Store) DeleteSchedule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sched := range s.data.Schedules {
		if sched.ID == id {
			s.data.Schedules = append(s.data.Schedules[:i], s.data.Schedules[i+1:]...)
			s.clearBroadcastsForIDLocked(id)
			return s.save()
		}
	}
	return fmt.Errorf("schedule %s not found", id)
}

// Overrides returns all one-time overrides.
func (s *Store) Overrides() []Override {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Override, len(s.data.Overrides))
	copy(out, s.data.Overrides)
	return out
}

// CreateOverride adds a one-time event.
func (s *Store) CreateOverride(o Override) (Override, error) {
	o, err := normalizeOverride(o)
	if err != nil {
		return Override{}, err
	}
	o.ID = newID()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Overrides = append(s.data.Overrides, o)
	return o, s.save()
}

func normalizeOverride(o Override) (Override, error) {
	if o.Name == "" {
		return Override{}, fmt.Errorf("name is required")
	}
	// If client provided a wall-clock + timezone, convert to UTC.
	if o.WallClock != "" {
		tz := o.Timezone
		if tz == "" {
			tz = "UTC"
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return Override{}, fmt.Errorf("invalid timezone %q: %w", tz, err)
		}
		// HTML datetime-local is "YYYY-MM-DDTHH:MM" (no seconds, no zone).
		t, err := time.ParseInLocation("2006-01-02T15:04", o.WallClock, loc)
		if err != nil {
			// Try with seconds.
			t, err = time.ParseInLocation("2006-01-02T15:04:05", o.WallClock, loc)
			if err != nil {
				return Override{}, fmt.Errorf("invalid wallClock %q: %w", o.WallClock, err)
			}
		}
		o.StartTime = t.UTC()
		o.WallClock = ""
	}
	if o.StartTime.IsZero() {
		return Override{}, fmt.Errorf("start time is required")
	}
	if o.DurationMin <= 0 {
		o.DurationMin = 120
	}
	if o.Privacy == "" {
		o.Privacy = "unlisted"
	}
	return o, nil
}

// UpdateOverride replaces a one-time override by ID.
func (s *Store) UpdateOverride(o Override) (Override, error) {
	o, err := normalizeOverride(o)
	if err != nil {
		return Override{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.data.Overrides {
		if existing.ID == o.ID {
			o.ID = existing.ID
			s.data.Overrides[i] = o
			s.clearBroadcastsForIDLocked(o.ID)
			return o, s.save()
		}
	}
	return Override{}, fmt.Errorf("override %s not found", o.ID)
}

// DeleteOverride removes an override by ID.
func (s *Store) DeleteOverride(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, o := range s.data.Overrides {
		if o.ID == id {
			s.data.Overrides = append(s.data.Overrides[:i], s.data.Overrides[i+1:]...)
			s.clearBroadcastsForIDLocked(id)
			return s.save()
		}
	}
	return fmt.Errorf("override %s not found", id)
}

// SetBroadcastID stores the YouTube broadcast ID for an event.
func (s *Store) SetBroadcastID(eventKey, broadcastID, streamID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Broadcasts[eventKey] = broadcastID
	if streamID != "" {
		s.data.Streams[eventKey] = streamID
	}
	return s.save()
}

// GetBroadcastID returns the YouTube broadcast ID for an event key.
func (s *Store) GetBroadcastID(eventKey string) (broadcastID, streamID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Broadcasts[eventKey], s.data.Streams[eventKey]
}

// ClearBroadcast removes a broadcast mapping (after completion).
func (s *Store) ClearBroadcast(eventKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Broadcasts, eventKey)
	delete(s.data.Streams, eventKey)
	return s.save()
}

// SkipEvent suppresses one computed event occurrence until its event window
// ends. Used when an operator stops a scheduled stream early; without this,
// the scheduler would see the still-due occurrence and restart it.
func (s *Store) SkipEvent(eventKey string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Skipped == nil {
		s.data.Skipped = make(map[string]time.Time)
	}
	s.data.Skipped[eventKey] = until.UTC()
	delete(s.data.Broadcasts, eventKey)
	delete(s.data.Streams, eventKey)
	return s.save()
}

// NextEvents computes the next upcoming events from all schedules and overrides.
func (s *Store) NextEvents(count int, after time.Time) []Event {
	s.mu.Lock()
	schedules := make([]Schedule, len(s.data.Schedules))
	copy(schedules, s.data.Schedules)
	overrides := make([]Override, len(s.data.Overrides))
	copy(overrides, s.data.Overrides)
	broadcasts := make(map[string]string)
	streams := make(map[string]string)
	skipped := make(map[string]time.Time)
	for k, v := range s.data.Broadcasts {
		broadcasts[k] = v
	}
	for k, v := range s.data.Streams {
		streams[k] = v
	}
	for k, v := range s.data.Skipped {
		skipped[k] = v
	}
	s.mu.Unlock()

	var events []Event

	// Add recurring schedule events for the next 14 days. Start the recurrence
	// search one duration in the past so a schedule that just became due stays
	// visible to the scheduler until its event window ends.
	horizon := after.Add(14 * 24 * time.Hour)
	for _, sched := range schedules {
		if !sched.Enabled {
			continue
		}
		duration := time.Duration(sched.DurationMin) * time.Minute
		searchAfter := after.Add(-duration)
		for _, t := range nextOccurrences(sched, searchAfter, horizon) {
			if t.Add(duration).Before(after) {
				continue
			}
			key := EventKey(sched.ID, t)
			if until, ok := skipped[key]; ok && until.After(after) {
				continue
			}
			ev := Event{
				ScheduleID:  sched.ID,
				Name:        sched.Name,
				StartTime:   t,
				DurationMin: sched.DurationMin,
				PresetID:    sched.PresetID,
				Title:       sched.Title,
				Description: sched.Description,
				Privacy:     sched.Privacy,
				BroadcastID: broadcasts[key],
				StreamID:    streams[key],
			}
			events = append(events, ev)
		}
	}

	// Add overrides that haven't passed.
	for _, o := range overrides {
		endTime := o.StartTime.Add(time.Duration(o.DurationMin) * time.Minute)
		if endTime.Before(after) {
			continue
		}
		key := EventKey(o.ID, o.StartTime)
		if until, ok := skipped[key]; ok && until.After(after) {
			continue
		}
		events = append(events, Event{
			OverrideID:  o.ID,
			Name:        o.Name,
			StartTime:   o.StartTime,
			DurationMin: o.DurationMin,
			PresetID:    o.PresetID,
			Title:       o.Title,
			Description: o.Description,
			Privacy:     o.Privacy,
			BroadcastID: broadcasts[key],
			StreamID:    streams[key],
		})
	}

	sort.Slice(events, func(i, j int) bool {
		return events[i].StartTime.Before(events[j].StartTime)
	})

	if len(events) > count {
		events = events[:count]
	}
	return events
}

// EventKey returns a stable key for mapping events to YouTube broadcasts.
func EventKey(id string, startTime time.Time) string {
	return id + ":" + startTime.UTC().Format(time.RFC3339)
}

func (s *Store) clearBroadcastsForIDLocked(id string) {
	prefix := id + ":"
	for key := range s.data.Broadcasts {
		if strings.HasPrefix(key, prefix) {
			delete(s.data.Broadcasts, key)
		}
	}
	for key := range s.data.Streams {
		if strings.HasPrefix(key, prefix) {
			delete(s.data.Streams, key)
		}
	}
	for key := range s.data.Skipped {
		if strings.HasPrefix(key, prefix) {
			delete(s.data.Skipped, key)
		}
	}
}

// nextOccurrences computes the next firings of a recurring schedule using
// robfig/cron/v3. We build a standard 5-field cron expression
// ("min hour * * dow") from our days+time+timezone model and walk Next()
// until we pass the horizon.
//
// robfig/cron handles DST transitions, leap days, and timezone math
// correctly — things our previous hand-rolled implementation had to be
// trusted on without test coverage.
func nextOccurrences(sched Schedule, after, horizon time.Time) []time.Time {
	loc, err := time.LoadLocation(sched.Timezone)
	if err != nil {
		return nil
	}
	parts := strings.SplitN(sched.Time, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	hour, _ := strconv.Atoi(parts[0])
	minute, _ := strconv.Atoi(parts[1])

	// Build the day-of-week field. robfig/cron accepts 0-6 (Sun-Sat),
	// matching our parsed time.Weekday values.
	var days []string
	for _, d := range sched.Days {
		if wd, ok := parseWeekday(d); ok {
			days = append(days, strconv.Itoa(int(wd)))
		}
	}
	if len(days) == 0 {
		return nil
	}
	expr := fmt.Sprintf("%d %d * * %s", minute, hour, strings.Join(days, ","))

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(expr)
	if err != nil {
		return nil
	}

	// Walk Next() in the schedule's timezone. Cron's Next interprets the
	// passed time in its zone, so we hand it a time anchored to loc.
	var times []time.Time
	t := after.In(loc)
	for {
		t = schedule.Next(t)
		if t.IsZero() || !t.Before(horizon) {
			break
		}
		times = append(times, t.UTC())
	}
	return times
}

func parseWeekday(s string) (time.Weekday, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sunday":
		return time.Sunday, true
	case "monday":
		return time.Monday, true
	case "tuesday":
		return time.Tuesday, true
	case "wednesday":
		return time.Wednesday, true
	case "thursday":
		return time.Thursday, true
	case "friday":
		return time.Friday, true
	case "saturday":
		return time.Saturday, true
	default:
		return 0, false
	}
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.file, data, 0600)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
