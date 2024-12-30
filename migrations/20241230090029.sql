-- Create "users" table
CREATE TABLE `users` (
  `id` integer NULL PRIMARY KEY AUTOINCREMENT,
  `created_at` datetime NULL,
  `updated_at` datetime NULL,
  `deleted_at` datetime NULL,
  `name` text NULL,
  `email` text NULL,
  `age` integer NULL,
  `birthday` datetime NULL,
  `member_number` text NULL,
  `activated_at` datetime NULL
);
-- Create index "idx_users_deleted_at" to table: "users"
CREATE INDEX `idx_users_deleted_at` ON `users` (`deleted_at`);
-- Create "products" table
CREATE TABLE `products` (
  `id` integer NULL PRIMARY KEY AUTOINCREMENT,
  `created_at` datetime NULL,
  `updated_at` datetime NULL,
  `deleted_at` datetime NULL,
  `code` text NULL,
  `price` integer NULL
);
-- Create index "idx_products_deleted_at" to table: "products"
CREATE INDEX `idx_products_deleted_at` ON `products` (`deleted_at`);
