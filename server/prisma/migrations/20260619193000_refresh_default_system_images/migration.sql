-- Refresh default system image catalog.
-- Keep custom admin-created images, but remove the old built-in defaults that
-- are no longer part of the supported catalog.

DELETE FROM "system_images"
WHERE "remote_alias" IN (
  'images:alpine/3.19/cloud',
  'images:alpine/3.20/cloud',
  'images:fedora/41/cloud',
  'images:fedora/42/cloud',
  'images:kali/cloud',
  'images:opensuse/15.6/cloud',
  'images:opensuse/tumbleweed/cloud',
  'images:oracle/8/cloud',
  'images:oracle/9/cloud',
  'images:ubuntu/25.04/cloud'
);

INSERT INTO "system_images" ("name", "remote_alias", "os_type", "architecture", "instance_type", "icon", "sort_order", "updated_at") VALUES
-- Alpine
('Alpine 3.22', 'images:alpine/3.22/cloud', 'Linux', 'x86_64', 'both', 'alpine', 104, NOW()),
('Alpine 3.22', 'images:alpine/3.22/cloud/arm64', 'Linux', 'aarch64', 'both', 'alpine', 105, NOW()),
('Alpine 3.23', 'images:alpine/3.23/cloud', 'Linux', 'x86_64', 'both', 'alpine', 108, NOW()),
('Alpine 3.23', 'images:alpine/3.23/cloud/arm64', 'Linux', 'aarch64', 'both', 'alpine', 109, NOW()),
('Alpine 3.24', 'images:alpine/3.24/cloud', 'Linux', 'x86_64', 'both', 'alpine', 112, NOW()),
('Alpine 3.24', 'images:alpine/3.24/cloud/arm64', 'Linux', 'aarch64', 'both', 'alpine', 113, NOW()),
-- Debian
('Debian 12 (Bookworm)', 'images:debian/12/cloud', 'Linux', 'x86_64', 'both', 'debian', 202, NOW()),
('Debian 12 (Bookworm)', 'images:debian/12/cloud/arm64', 'Linux', 'aarch64', 'both', 'debian', 203, NOW()),
('Debian 13 (Trixie)', 'images:debian/13/cloud', 'Linux', 'x86_64', 'both', 'debian', 204, NOW()),
('Debian 13 (Trixie)', 'images:debian/13/cloud/arm64', 'Linux', 'aarch64', 'both', 'debian', 205, NOW()),
('Debian 14 (Forky)', 'images:debian/14/cloud', 'Linux', 'x86_64', 'both', 'debian', 206, NOW()),
('Debian 14 (Forky)', 'images:debian/14/cloud/arm64', 'Linux', 'aarch64', 'both', 'debian', 207, NOW()),
-- Ubuntu
('Ubuntu 24.04 (Noble)', 'images:ubuntu/noble/cloud', 'Linux', 'x86_64', 'both', 'ubuntu', 302, NOW()),
('Ubuntu 24.04 (Noble)', 'images:ubuntu/noble/cloud/arm64', 'Linux', 'aarch64', 'both', 'ubuntu', 303, NOW()),
('Ubuntu 25.10 (Questing)', 'images:ubuntu/25.10/cloud', 'Linux', 'x86_64', 'both', 'ubuntu', 304, NOW()),
('Ubuntu 25.10 (Questing)', 'images:ubuntu/25.10/cloud/arm64', 'Linux', 'aarch64', 'both', 'ubuntu', 305, NOW()),
('Ubuntu 26.04 (Resolute)', 'images:ubuntu/26.04/cloud', 'Linux', 'x86_64', 'both', 'ubuntu', 306, NOW()),
('Ubuntu 26.04 (Resolute)', 'images:ubuntu/26.04/cloud/arm64', 'Linux', 'aarch64', 'both', 'ubuntu', 307, NOW())
ON CONFLICT ("remote_alias") DO UPDATE SET
  "name" = EXCLUDED."name",
  "os_type" = EXCLUDED."os_type",
  "architecture" = EXCLUDED."architecture",
  "instance_type" = EXCLUDED."instance_type",
  "icon" = EXCLUDED."icon",
  "sort_order" = EXCLUDED."sort_order",
  "hidden" = false,
  "updated_at" = NOW();
