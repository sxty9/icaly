// Package rights enumerates the fine-grained rights icaly declares to the holistic
// rights standard. Each constant is the Linux group backing one permission in
// permissions/icaly.json — keep the three in sync (this file ⇄ the manifest ⇄ the UI
// right constants). Enforcement uses auth.User.Can, i.e. isAdmin || group ∈ groups; a
// host without privleg has empty hp_* groups, so non-admins reduce to admin-only.
package rights

const (
	// GroupView backs calendar:view — read own + shared calendars, ICS/CalDAV/JMAP read,
	// subscribe to external feeds, RSVP to invitations. Default-on for every user.
	GroupView = "hp_icaly_view"
	// GroupEdit backs calendar:edit — create/update/delete events in own + write-shared
	// calendars, import, CalDAV/JMAP writes. Default-on for every user.
	GroupEdit = "hp_icaly_edit"
	// GroupShare backs calendar:share — extra calendars, per-calendar ACLs, app passwords,
	// public/secret feed tokens.
	GroupShare = "hp_icaly_share"
	// GroupInvite backs calendar:invite — invite EXTERNAL email addresses (iMIP egress).
	// External-invite egress additionally requires hp_mail_send (see scheduling, M6).
	GroupInvite = "hp_icaly_invite"
	// GroupDelegate backs calendar:delegate — act on a delegated calendar (RFC 6638).
	GroupDelegate = "hp_icaly_delegate"
	// GroupAdmin backs admin:admin — any user's calendars + instance resources (rooms,
	// equipment) + audit. Dangerous.
	GroupAdmin = "hp_icaly_admin"
)
