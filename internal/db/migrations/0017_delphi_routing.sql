-- Delphi routing (docs/DELPHI.md, "Tiers" and "Fallback on refusal").
--
-- profile is the model-catalog alias a delegation was resolved from ("implementer",
-- "planner.frontier"), so spend can be read per tier. fallback_of is the ref of the
-- delegation whose model refused the same request, when this one is its fallback. Both are
-- names, not content.
ALTER TABLE delphi_delegations
    ADD COLUMN profile     text NOT NULL DEFAULT '',
    ADD COLUMN fallback_of text NOT NULL DEFAULT '';
