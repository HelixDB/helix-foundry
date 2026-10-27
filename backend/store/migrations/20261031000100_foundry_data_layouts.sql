-- +goose Up
-- How a workspace's Data tree is arranged: the website's JSON (names,
-- folders, order), opaque to the backend beyond being an object of at most
-- 64 KiB. revision counts writes, so a write that saw an older one is
-- refused; a workspace with no row has revision 0.
CREATE TABLE foundry_data_layouts (
  workspace_id       CHAR(26)    NOT NULL PRIMARY KEY,
  layout             JSON        NOT NULL,
  revision           BIGINT      NOT NULL,
  updated_by_user_id CHAR(26)    NULL,
  updated_at         DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
);

-- +goose Down
DROP TABLE foundry_data_layouts;
