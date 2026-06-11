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
	KTypeCourse      = "lms.course"
	KTypeModule      = "lms.module"
	KTypeLesson      = "lms.lesson"
	KTypeEnrollment  = "lms.enrollment"
	KTypeQuiz        = "lms.quiz"
	KTypeAssignment  = "lms.assignment"
	KTypeProgress    = "lms.progress"
	KTypeCertificate = "lms.certificate"
)

// Canonical workflow names.
const (
	WorkflowCourse     = "lms.course.lifecycle"
	WorkflowEnrollment = "lms.enrollment.lifecycle"
	WorkflowAssignment = "lms.assignment.lifecycle"
)

// Assignment status values. The KType stores `status` as an enum so the
// web UI and the lms.submit_assignment agent tool share the same
// vocabulary. Terminal states are `approved` (reviewer accepted) and
// `returned` (reviewer sent back for revision); the latter loops back
// to `submitted` via a second `submit_for_review` transition, which
// re-triggers the approval chain for the reviewer.
const (
	AssignmentStatusDraft     = "draft"
	AssignmentStatusSubmitted = "submitted"
	AssignmentStatusApproved  = "approved"
	AssignmentStatusReturned  = "returned"
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

// lessonSchema is version 2: content_type gains embed / scorm_12 /
// scorm_2004 / xapi (Deliverables 2 & 3) and a structured `blocks`
// array backs the rich block-based editor (Deliverable 5). The legacy
// scalar `content` field is retained so version-1 lessons render
// unchanged — `blocks` is additive and optional.
var lessonSchema = []byte(`{
  "name": "lms.lesson",
  "version": 2,
  "fields": [
    {"name": "module_id", "type": "ref", "ktype": "lms.module", "required": true},
    {"name": "title", "type": "string", "required": true, "max_length": 200},
    {"name": "content_type", "type": "enum", "values": ["text", "video", "markdown", "quiz", "assignment", "embed", "scorm_12", "scorm_2004", "xapi"], "default": "text"},
    {"name": "content", "type": "text"},
    {"name": "blocks", "type": "array"},
    {"name": "scorm_package_key", "type": "string", "max_length": 512},
    {"name": "order", "type": "number", "min": 0}
  ],
  "views": {
    "list": {"columns": ["module_id", "order", "title", "content_type"]},
    "form": {"sections": [{"title": "Lesson", "fields": ["module_id", "title", "content_type", "content", "blocks", "order"]}]}
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
    {"name": "due_date", "type": "date"},
    {"name": "reviewer_id", "type": "ref", "ktype": "hr.employee"},
    {"name": "status", "type": "enum", "values": ["draft", "submitted", "approved", "returned"], "default": "draft"}
  ],
  "views": {
    "list": {"columns": ["lesson_id", "title", "due_date", "reviewer_id", "status"]},
    "form": {"sections": [{"title": "Assignment", "fields": ["lesson_id", "title", "description", "due_date", "reviewer_id", "status"]}]},
    "kanban": {"group_by": "status", "card_title": "title", "card_subtitle": "reviewer_id"}
  },
  "cards": {"summary": "Assignment: {{title}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]},
  "workflow": {
    "name": "lms.assignment.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "submitted", "approved", "returned"],
    "transitions": [
      {"from": ["draft"], "to": "submitted", "action": "submit_for_review", "post": ["approvals.request"]},
      {"from": ["submitted"], "to": "approved", "action": "approve"},
      {"from": ["submitted"], "to": "returned", "action": "return_for_revision"},
      {"from": ["returned"], "to": "submitted", "action": "submit_for_review", "post": ["approvals.request"]}
    ]
  }
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

var certificateSchema = []byte(`{
  "name": "lms.certificate",
  "version": 1,
  "fields": [
    {"name": "enrollment_id", "type": "ref", "ktype": "lms.enrollment", "required": true},
    {"name": "course_id", "type": "ref", "ktype": "lms.course", "required": true},
    {"name": "learner_id", "type": "ref", "ktype": "user", "required": true},
    {"name": "certificate_number", "type": "string", "required": true, "max_length": 64},
    {"name": "issued_at", "type": "datetime", "required": true},
    {"name": "template_id", "type": "string", "max_length": 64}
  ],
  "views": {
    "list": {"columns": ["certificate_number", "course_id", "learner_id", "issued_at"]},
    "form": {"sections": [{"title": "Certificate", "fields": ["enrollment_id", "course_id", "learner_id", "certificate_number", "issued_at", "template_id"]}]}
  },
  "cards": {"summary": "{{certificate_number}} — {{course_id}}"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

var learningPathSchema = []byte(`{
  "name": "lms.learning_path",
  "version": 1,
  "fields": [
    {"name": "title", "type": "string", "required": true, "max_length": 200},
    {"name": "description", "type": "text"},
    {"name": "status", "type": "enum", "values": ["draft", "published", "archived"], "default": "draft"},
    {"name": "target_roles", "type": "array"},
    {"name": "estimated_duration_hours", "type": "number", "min": 0},
    {"name": "difficulty", "type": "enum", "values": ["beginner", "intermediate", "advanced"], "default": "beginner"}
  ],
  "views": {
    "list": {"columns": ["title", "status", "difficulty", "estimated_duration_hours"]},
    "form": {"sections": [{"title": "Learning Path", "fields": ["title", "description", "status", "target_roles", "estimated_duration_hours", "difficulty"]}]}
  },
  "cards": {"summary": "{{title}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

var learningPathCourseSchema = []byte(`{
  "name": "lms.learning_path_course",
  "version": 1,
  "fields": [
    {"name": "learning_path_id", "type": "ref", "ktype": "lms.learning_path", "required": true},
    {"name": "course_id", "type": "ref", "ktype": "lms.course", "required": true},
    {"name": "sequence_order", "type": "number", "min": 0},
    {"name": "is_mandatory", "type": "boolean", "default": true},
    {"name": "prerequisite_course_ids", "type": "array"}
  ],
  "views": {
    "list": {"columns": ["learning_path_id", "sequence_order", "course_id", "is_mandatory"]},
    "form": {"sections": [{"title": "Path Course", "fields": ["learning_path_id", "course_id", "sequence_order", "is_mandatory", "prerequisite_course_ids"]}]}
  },
  "cards": {"summary": "{{course_id}} (#{{sequence_order}})"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

var learningPathEnrollmentSchema = []byte(`{
  "name": "lms.learning_path_enrollment",
  "version": 1,
  "fields": [
    {"name": "learning_path_id", "type": "ref", "ktype": "lms.learning_path", "required": true},
    {"name": "user_id", "type": "ref", "ktype": "user", "required": true},
    {"name": "status", "type": "enum", "values": ["enrolled", "in_progress", "completed"], "default": "enrolled"},
    {"name": "source", "type": "enum", "values": ["manual", "auto"], "default": "manual"},
    {"name": "started_at", "type": "datetime"},
    {"name": "completed_at", "type": "datetime"}
  ],
  "views": {
    "list": {"columns": ["learning_path_id", "user_id", "status", "source", "completed_at"]},
    "form": {"sections": [{"title": "Path Enrollment", "fields": ["learning_path_id", "user_id", "status", "source", "started_at", "completed_at"]}]},
    "kanban": {"group_by": "status", "card_title": "learning_path_id", "card_subtitle": "user_id"}
  },
  "cards": {"summary": "{{user_id}} — {{learning_path_id}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

var badgeSchema = []byte(`{
  "name": "lms.badge",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 120},
    {"name": "description", "type": "text"},
    {"name": "icon", "type": "string", "max_length": 200},
    {"name": "criteria_type", "type": "enum", "values": ["course_complete", "path_complete", "quiz_score", "streak"], "required": true},
    {"name": "criteria_value", "type": "json"},
    {"name": "active", "type": "boolean", "default": true}
  ],
  "views": {
    "list": {"columns": ["name", "criteria_type", "active"]},
    "form": {"sections": [{"title": "Badge", "fields": ["name", "description", "icon", "criteria_type", "criteria_value", "active"]}]}
  },
  "cards": {"summary": "{{name}} ({{criteria_type}})"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

var userBadgeSchema = []byte(`{
  "name": "lms.user_badge",
  "version": 1,
  "fields": [
    {"name": "user_id", "type": "ref", "ktype": "user", "required": true},
    {"name": "badge_id", "type": "ref", "ktype": "lms.badge", "required": true},
    {"name": "earned_at", "type": "datetime", "required": true}
  ],
  "views": {
    "list": {"columns": ["user_id", "badge_id", "earned_at"]},
    "form": {"sections": [{"title": "User Badge", "fields": ["user_id", "badge_id", "earned_at"]}]}
  },
  "cards": {"summary": "{{user_id}} — {{badge_id}}"},
  "permissions": {"read": ["tenant.member"], "write": ["lms.admin", "tenant.admin"]}
}`)

var discussionThreadSchema = []byte(`{
  "name": "lms.discussion_thread",
  "version": 1,
  "fields": [
    {"name": "course_id", "type": "ref", "ktype": "lms.course", "required": true},
    {"name": "lesson_id", "type": "ref", "ktype": "lms.lesson"},
    {"name": "author_id", "type": "ref", "ktype": "user", "required": true},
    {"name": "title", "type": "string", "required": true, "max_length": 200},
    {"name": "body", "type": "text"},
    {"name": "status", "type": "enum", "values": ["open", "resolved", "closed"], "default": "open"},
    {"name": "pinned", "type": "boolean", "default": false}
  ],
  "views": {
    "list": {"columns": ["title", "course_id", "status", "pinned"]},
    "form": {"sections": [{"title": "Thread", "fields": ["course_id", "lesson_id", "author_id", "title", "body", "status", "pinned"]}]}
  },
  "cards": {"summary": "{{title}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["tenant.member"]}
}`)

var discussionReplySchema = []byte(`{
  "name": "lms.discussion_reply",
  "version": 1,
  "fields": [
    {"name": "thread_id", "type": "ref", "ktype": "lms.discussion_thread", "required": true},
    {"name": "author_id", "type": "ref", "ktype": "user", "required": true},
    {"name": "body", "type": "text", "required": true},
    {"name": "is_answer", "type": "boolean", "default": false}
  ],
  "views": {
    "list": {"columns": ["thread_id", "author_id", "is_answer"]},
    "form": {"sections": [{"title": "Reply", "fields": ["thread_id", "author_id", "body", "is_answer"]}]}
  },
  "cards": {"summary": "Reply by {{author_id}}"},
  "permissions": {"read": ["tenant.member"], "write": ["tenant.member"]}
}`)

// All returns every Phase E LMS KType as a freshly-constructed slice.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeCourse, Version: 1, Schema: courseSchema},
		{Name: KTypeModule, Version: 1, Schema: moduleSchema},
		{Name: KTypeLesson, Version: 2, Schema: lessonSchema},
		{Name: KTypeEnrollment, Version: 1, Schema: enrollmentSchema},
		{Name: KTypeQuiz, Version: 1, Schema: quizSchema},
		{Name: KTypeAssignment, Version: 1, Schema: assignmentSchema},
		{Name: KTypeProgress, Version: 1, Schema: progressSchema},
		{Name: KTypeCertificate, Version: 1, Schema: certificateSchema},
		{Name: KTypeLearningPath, Version: 1, Schema: learningPathSchema},
		{Name: KTypeLearningPathCourse, Version: 1, Schema: learningPathCourseSchema},
		{Name: KTypeLearningPathEnrollment, Version: 1, Schema: learningPathEnrollmentSchema},
		{Name: KTypeBadge, Version: 1, Schema: badgeSchema},
		{Name: KTypeUserBadge, Version: 1, Schema: userBadgeSchema},
		{Name: KTypeDiscussionThread, Version: 1, Schema: discussionThreadSchema},
		{Name: KTypeDiscussionReply, Version: 1, Schema: discussionReplySchema},
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
		if err := registry.RegisterIfChanged(ctx, kt); err != nil {
			return fmt.Errorf("lms: register %s: %w", kt.Name, err)
		}
	}
	return nil
}
