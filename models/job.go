package models

import "time"

type Job struct {
	ID        string
	Status    string // pending, running, done, error
	FilePath  string
	Error     string
	CreatedAt time.Time
}
