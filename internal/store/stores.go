package store

import (
	"mail_go/internal/db"

	"gorm.io/gorm"
)

// Stores aggregates all store interfaces for convenient access.
type Stores struct {
	Users       UserStore
	Mails       MailStore
	Domains     DomainStore
	Attachments AttachmentStore
	Bans        BanStore
}

// NewStores creates a new Stores instance with all GORM-backed implementations.
func NewStores(database *gorm.DB) *Stores {
	return &Stores{
		Users:       newUserStore(database),
		Mails:       newMailStore(database),
		Domains:     newDomainStore(database),
		Attachments: newAttachmentStore(database),
		Bans:        newBanStore(database),
	}
}

// Ensure models are referenced (prevents unused import errors).
var _ = db.User{}
var _ = db.Domain{}
var _ = db.Message{}
var _ = db.Attachment{}
var _ = db.BanEntry{}
