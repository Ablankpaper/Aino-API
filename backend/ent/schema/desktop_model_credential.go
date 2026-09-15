package schema

import (
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// DesktopModelCredential holds time-limited, revocable API keys issued to
// desktop clients for managed model access.
type DesktopModelCredential struct {
	ent.Schema
}

func (DesktopModelCredential) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "desktop_model_credentials"},
	}
}

func (DesktopModelCredential) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (DesktopModelCredential) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("user_id"),
		field.String("device_id").MaxLen(128),
		field.String("connection_grant_id").MaxLen(128),
		field.String("session_family_id").MaxLen(128),
		field.Int64("token_version"),
		field.Int64("group_id"),
		field.String("model_id").MaxLen(128),
		field.Int64("api_key_id"),
		field.Time("expires_at"),
		field.Time("revoked_at").Optional().Nillable(),
		field.String("revoke_reason").MaxLen(100).Optional(),
	}
}

func (DesktopModelCredential) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", Group.Type).Ref("desktop_model_credentials").Field("group_id").Unique().Required(),
		edge.From("user", User.Type).
			Ref("desktop_model_credentials").
			Field("user_id").
			Unique().
			Required(),
		edge.From("api_key", APIKey.Type).
			Ref("desktop_model_credentials").
			Field("api_key_id").
			Unique().
			Required(),
	}
}

func (DesktopModelCredential) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "device_id", "connection_grant_id", "session_family_id", "group_id").Unique().Annotations(entsql.IndexWhere("revoked_at IS NULL")),
		index.Fields("session_family_id"),
		index.Fields("user_id", "device_id"),
		index.Fields("api_key_id").Unique(),
		index.Fields("user_id", "created_at"),
	}
}
