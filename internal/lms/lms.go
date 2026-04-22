// Package lms holds the Phase E LMS KType definitions: courses,
// modules, lessons, enrollments, quizzes, assignments, and progress.
// The Phase E starter leans on the KRecord store for persistence and
// the workflow engine for state transitions; only lesson progress has
// a dedicated typed home (migrations/000007_lms.sql) so per-user
// per-lesson progress projections stay cheap and indexable.
package lms

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// KType identifiers.
const (
	KTypeCourse     = "lms.course"
	KTypeModule     = "lms.module"
	KTypeLesson     = "lms.lesson"
	KTypeEnrollment = "lms.enrollment"
	KTypeQuiz       = "lms.quiz"
	KTypeAssignment = "lms.assignment"
	KTypeProgress   = "lms.progress"
)

// Canonical workflow names.
const (
	WorkflowCourse     = "lms.course.lifecycle"
	WorkflowEnrollment = "lms.enrollment.lifecycle"
)

var courseSchema = []byte(`{
  "name": "lms.course",
  "version": 1,
  "fields": [
    {"name": "title", "type": "string", "required": true, "max_length": 200},
    {"name": "description", "type": "text"},
    {"name": "status", "type": "enum", "values": ["draft", "published", "archived"], "default": "draft"},
    {"name": "instructor_id", "type": "ref", "ktype": "hr.employee"}
  ],
  "views": {
    "list": {"columns": ["title", "instructor_id", "status"]},
    "form": {"sections": [{"title": "Course", "fields": ["title", "description", "instructor_id", "status"]}]}
  },
  "cards": {"summary": "{{title}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]},
  "workflow": {
    "name": "lms.course.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "published", "archived"],
    "transitions": [
      {"from": ["draft"], "to": "published", "action": "publish"},
      {"from": ["published"], "to": "archived", "action": "archive"}
    ]
  }
}`)

var moduleSchema = []byte(`{
  "name": "lms.module",
  "version": 1,
  "fields": [
    {"name": "course_id", "type": "ref", "ktype": "lms.course", "required": true},
    {"name": "title", "type": "string", "required": true, "max_length": 200},
    {"name": "order", "type": "number", "min": 0}
  ],
  "views": {
    "list": {"columns": ["course_id", "order", "title"]},
    "form": {"sections": [{"title": "Module", "fields": ["course_id", "title", "order"]}]}
  },
  "cards": {"summary": "{{title}}"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

var lessonSchema = []byte(`{
  "name": "lms.lesson",
  "version": 1,
  "fields": [
    {"name": "module_id", "type": "ref", "ktype": "lms.module", "required": true},
    {"name": "title", "type": "string", "required": true, "max_length": 200},
    {"name": "content_type", "type": "enum", "values": ["text", "video", "markdown", "quiz", "assignment"], "default": "text"},
    {"name": "content", "type": "text"},
    {"name": "order", "type": "number", "min": 0}
  ],
  "views": {
    "list": {"columns": ["module_id", "order", "title", "content_type"]},
    "form": {"sections": [{"title": "Lesson", "fields": ["module_id", "title", "content_type", "content", "order"]}]}
  },
  "cards": {"summary": "{{title}} ({{content_type}})"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

var enrollmentSchema = []byte(`{
  "name": "lms.enrollment",
  "version": 1,
  "fields": [
    {"name": "user_id", "type": "ref", "ktype": "user", "required": true},
    {"name": "course_id", "type": "ref", "ktype": "lms.course", "required": true},
    {"name": "enrolled_at", "type": "datetime"},
    {"name": "completed_at", "type": "datetime"},
    {"name": "status", "type": "enum", "values": ["enrolled", "in_progress", "completed", "dropped"], "default": "enrolled"}
  ],
  "views": {
    "list": {"columns": ["user_id", "course_id", "status", "enrolled_at", "completed_at"]},
    "form": {"sections": [{"title": "Enrollment", "fields": ["user_id", "course_id", "enrolled_at", "completed_at", "status"]}]},
    "kanban": {"group_by": "status", "card_title": "course_id", "card_subtitle": "user_id"}
  },
  "cards": {"summary": "{{user_id}} — {{course_id}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]},
  "workflow": {
    "name": "lms.enrollment.lifecycle",
    "initial_state": "enrolled",
    "states": ["enrolled", "in_progress", "completed", "dropped"],
    "transitions": [
      {"from": ["enrolled"], "to": "in_progress", "action": "start"},
      {"from": ["in_progress"], "to": "completed", "action": "complete"},
      {"from": ["enrolled", "in_progress"], "to": "dropped", "action": "drop"}
    ]
  }
}`)

var quizSchema = []byte(`{
  "name": "lms.quiz",
  "version": 1,
  "fields": [
    {"name": "lesson_id", "type": "ref", "ktype": "lms.lesson", "required": true},
    {"name": "title", "type": "string", "max_length": 200},
    {"name": "questions", "type": "array"},
    {"name": "pass_threshold", "type": "number", "min": 0, "default": 0.7}
  ],
  "views": {
    "list": {"columns": ["lesson_id", "title", "pass_threshold"]},
    "form": {"sections": [{"title": "Quiz", "fields": ["lesson_id", "title", "questions", "pass_threshold"]}]}
  },
  "cards": {"summary": "Quiz: {{title}}"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

var assignmentSchema = []byte(`{
  "name": "lms.assignment",
  "version": 1,
  "fields": [
    {"name": "lesson_id", "type": "ref", "ktype": "lms.lesson", "required": true},
    {"name": "title", "type": "string", "required": true, "max_length": 200},
    {"name": "description", "type": "text"},
    {"name": "due_date", "type": "date"}
  ],
  "views": {
    "list": {"columns": ["lesson_id", "title", "due_date"]},
    "form": {"sections": [{"title": "Assignment", "fields": ["lesson_id", "title", "description", "due_date"]}]}
  },
  "cards": {"summary": "Assignment: {{title}}"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

var progressSchema = []byte(`{
  "name": "lms.progress",
  "version": 1,
  "fields": [
    {"name": "enrollment_id", "type": "ref", "ktype": "lms.enrollment", "required": true},
    {"name": "lesson_id", "type": "ref", "ktype": "lms.lesson", "required": true},
    {"name": "status", "type": "enum", "values": ["not_started", "in_progress", "completed"], "default": "not_started"},
    {"name": "score", "type": "number", "min": 0},
    {"name": "completed_at", "type": "datetime"}
  ],
  "views": {
    "list": {"columns": ["enrollment_id", "lesson_id", "status", "score", "completed_at"]},
    "form": {"sections": [{"title": "Progress", "fields": ["enrollment_id", "lesson_id", "status", "score", "completed_at"]}]}
  },
  "cards": {"summary": "{{lesson_id}} — {{status}} ({{score}})"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

// All returns every Phase E LMS KType as a freshly-constructed slice.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeCourse, Version: 1, Schema: courseSchema},
		{Name: KTypeModule, Version: 1, Schema: moduleSchema},
		{Name: KTypeLesson, Version: 1, Schema: lessonSchema},
		{Name: KTypeEnrollment, Version: 1, Schema: enrollmentSchema},
		{Name: KTypeQuiz, Version: 1, Schema: quizSchema},
		{Name: KTypeAssignment, Version: 1, Schema: assignmentSchema},
		{Name: KTypeProgress, Version: 1, Schema: progressSchema},
	}
}

func init() {
	for _, kt := range All() {
		if !json.Valid(kt.Schema) {
			panic(fmt.Sprintf("lms: embedded schema %q is not valid JSON", kt.Name))
		}
	}
}

// RegisterKTypes registers every Phase E LMS KType against the supplied
// registry. Idempotent: the underlying PGRegistry upserts on conflict.
func RegisterKTypes(ctx context.Context, registry ktype.Registry) error {
	for _, kt := range All() {
		if err := registry.Register(ctx, kt); err != nil {
			return fmt.Errorf("lms: register %s: %w", kt.Name, err)
		}
	}
	return nil
}
