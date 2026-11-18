package store

import "fmt"

// State is a mapping's lifecycle state (PLAN.md §6 state machine).
type State string

const (
	StatePendingPut    State = "pending_put"
	StateSynced        State = "synced"
	StatePendingDelete State = "pending_delete"
	StateDeleted       State = "deleted"
	StateUserDeleted   State = "user_deleted"
	StateSkipped       State = "skipped"
)

// States lists every state in a stable order.
var States = []State{StatePendingPut, StateSynced, StatePendingDelete, StateDeleted, StateUserDeleted, StateSkipped}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	for _, k := range States {
		if s == k {
			return true
		}
	}
	return false
}

// transitions lists the allowed moves to a different state. Staying in the
// same state (e.g. updating the ETag of a synced row, retrying a pending
// write) is always allowed.
//
//	(new)          -> pending_put                         (CreateMapping)
//	pending_put    -> synced            PUT ok / adopt
//	synced         -> pending_put       Google update
//	pending_put    -> pending_delete    cancelled before the PUT landed
//	synced         -> pending_delete    Google cancelled / self removed
//	pending_delete -> deleted           DELETE ok or 404
//	synced         -> user_deleted      href vanished from iCloud, not by us (§8.5)
//	pending_put    -> user_deleted      412/404 on update: user deleted it meanwhile
//	user_deleted   -> pending_put       recreate_on_reschedule or reconcile --restore only
//	user_deleted   -> deleted           Google cancelled; nothing left to delete in iCloud
//	deleted        -> pending_put       Google event reappears
//	any            -> skipped           filtered (organizer.self, eventType, no self attendee, href_collision, parked)
//	skipped        -> pending_put       filter no longer applies
//	skipped        -> deleted           Google cancelled a skipped event
//
// skipped is not a way around deleteMirror: a synced mirror that becomes
// filtered goes through pending_delete (CLAUDE.md rule 5); the syncer decides
// which. The store only guards against impossible jumps such as
// user_deleted -> synced.
var transitions = map[State][]State{
	StatePendingPut:    {StateSynced, StatePendingDelete, StateUserDeleted, StateSkipped},
	StateSynced:        {StatePendingPut, StatePendingDelete, StateUserDeleted, StateSkipped},
	StatePendingDelete: {StateDeleted, StateSkipped},
	StateDeleted:       {StatePendingPut, StateSkipped},
	StateUserDeleted:   {StatePendingPut, StateDeleted, StateSkipped},
	StateSkipped:       {StatePendingPut, StateDeleted},
}

// CanTransition reports whether from -> to is allowed.
func CanTransition(from, to State) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return true
	}
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// sourcesOf returns every state from which to is reachable (including to).
func sourcesOf(to State) []State {
	var out []State
	for _, from := range States {
		if CanTransition(from, to) {
			out = append(out, from)
		}
	}
	return out
}

func checkTransition(from, to State) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}
