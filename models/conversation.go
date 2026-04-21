package models

import "time"

type Conversation struct {
	ID        string    `gorm:"primaryKey;size:64" json:"id"`
	Title     string    `gorm:"size:255" json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (Conversation) TableName() string {
	return "conversations"
}
