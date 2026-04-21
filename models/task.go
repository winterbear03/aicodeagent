package models

import "time"

type Task struct {
	ID             string    `gorm:"primaryKey;size:64" json:"id"`
	ConversationID string    `gorm:"size:64;index" json:"conversation_id"` // 必须存在
	Input          string    `gorm:"type:text" json:"input"`
	Result         string    `gorm:"type:longtext" json:"result"`
	Status         string    `gorm:"size:20;default:'waiting'" json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (Task) TableName() string {
	return "tasks"
}
