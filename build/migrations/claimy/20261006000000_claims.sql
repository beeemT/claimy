-- +goose Up
CREATE TABLE app_groups (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    canonical_name VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_app_groups_canonical_name (canonical_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE apps (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    group_id BIGINT UNSIGNED NOT NULL,
    canonical_name VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_apps_group_name (group_id, canonical_name),
    UNIQUE KEY uq_apps_group_id (group_id, id),
    CONSTRAINT fk_apps_group FOREIGN KEY (group_id) REFERENCES app_groups (id)
        ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE claims (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    group_id BIGINT UNSIGNED NOT NULL,
    app_id BIGINT UNSIGNED NULL,
    owner_email VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    source ENUM('manual', 'ci') NOT NULL,
    gitlab_issuer VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
    gitlab_project_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    gitlab_job_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    gitlab_user_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    released_at DATETIME(6) NULL,
    revision INT UNSIGNED NOT NULL,
    PRIMARY KEY (id),
    KEY idx_claims_group_app_active_expiry (group_id, app_id, released_at, expires_at),
    KEY idx_claims_group_active_expiry (group_id, released_at, expires_at),
    CONSTRAINT fk_claims_group FOREIGN KEY (group_id) REFERENCES app_groups (id)
        ON DELETE RESTRICT ON UPDATE RESTRICT,
    CONSTRAINT fk_claims_app FOREIGN KEY (group_id, app_id) REFERENCES apps (group_id, id)
        ON DELETE RESTRICT ON UPDATE RESTRICT,
    CONSTRAINT chk_claims_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT chk_claims_release_after_creation CHECK (released_at IS NULL OR released_at >= created_at),
    CONSTRAINT chk_claims_revision_positive CHECK (revision > 0),
    CONSTRAINT chk_claims_ci_identity CHECK (
        (source = 'ci' AND gitlab_issuer IS NOT NULL AND gitlab_project_id IS NOT NULL AND
            gitlab_job_id IS NOT NULL AND gitlab_user_id IS NOT NULL) OR
        (source = 'manual' AND gitlab_issuer IS NULL AND gitlab_project_id IS NULL AND
            gitlab_job_id IS NULL AND gitlab_user_id IS NULL)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE claim_environments (
    claim_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    environment ENUM('sandbox', 'prod') NOT NULL,
    PRIMARY KEY (claim_id, environment),
    KEY idx_claim_environments_environment_claim (environment, claim_id),
    CONSTRAINT fk_claim_environments_claim FOREIGN KEY (claim_id) REFERENCES claims (id)
        ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE claim_versions (
    claim_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision INT UNSIGNED NOT NULL,
    valid_from DATETIME(6) NOT NULL,
    valid_to DATETIME(6) NULL,
    expires_at DATETIME(6) NOT NULL,
    released BOOLEAN NOT NULL,
    actor_email VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    actor_issuer VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor_subject VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    channel ENUM('rest', 'gitlab_ci', 'google_chat') NOT NULL,
    action ENUM('acquired', 'expiry_changed', 'released') NOT NULL,
    open_marker TINYINT GENERATED ALWAYS AS (IF(valid_to IS NULL, 1, NULL)) STORED,
    PRIMARY KEY (claim_id, revision),
    UNIQUE KEY uq_claim_versions_open (claim_id, open_marker),
    KEY idx_claim_versions_validity (valid_from, valid_to, claim_id, revision),
    KEY idx_claim_versions_closed_retention (valid_to, claim_id),
    CONSTRAINT fk_claim_versions_claim FOREIGN KEY (claim_id) REFERENCES claims (id)
        ON DELETE RESTRICT ON UPDATE RESTRICT,
    CONSTRAINT chk_claim_versions_expiry_after_start CHECK (expires_at > valid_from),
    CONSTRAINT chk_claim_versions_validity_order CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT chk_claim_versions_revision_positive CHECK (revision > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE request_results (
    principal_kind ENUM('gitlab_job', 'rest_user', 'google_chat_user') NOT NULL,
    principal_issuer VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    principal_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    payload_sha256 BINARY(32) NOT NULL,
    outcome ENUM('acquired', 'busy', 'released', 'expiry_changed') NOT NULL,
    claim_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    response_json JSON NOT NULL,
    created_at DATETIME(6) NOT NULL,
    retain_until DATETIME(6) NOT NULL,
    PRIMARY KEY (principal_kind, principal_issuer, principal_id, request_id),
    KEY idx_request_results_retain_until (retain_until),
    KEY idx_request_results_claim (claim_id),
    CONSTRAINT fk_request_results_claim FOREIGN KEY (claim_id) REFERENCES claims (id)
        ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- +goose Down
DROP TABLE request_results;
DROP TABLE claim_versions;
DROP TABLE claim_environments;
DROP TABLE claims;
DROP TABLE apps;
DROP TABLE app_groups;
