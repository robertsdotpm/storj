// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package satellitedb

import (
	"database/sql"
	"strconv"

	"github.com/golang/protobuf/proto"
	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/storj/internal/migrate"
	"storj.io/storj/pkg/pb"
	"storj.io/storj/satellite/console"
)

// ErrMigrate is for tracking migration errors
var ErrMigrate = errs.Class("migrate")

// CreateTables is a method for creating all tables for database
func (db *DB) CreateTables() error {
	switch db.driver {
	case "postgres":
		migration := db.PostgresMigration()
		return migration.Run(db.log.Named("migrate"), db.db)
	default:
		return migrate.Create("database", db.db)
	}
}

// PostgresMigration returns steps needed for migrating postgres database.
func (db *DB) PostgresMigration() *migrate.Migration {
	return &migrate.Migration{
		Table: "versions",
		Steps: []*migrate.Step{
			{
				// some databases may have already this done, although the version may not match
				Description: "Initial setup",
				Version:     0,
				Action: migrate.SQL{
					`CREATE TABLE IF NOT EXISTS accounting_raws (
						id bigserial NOT NULL,
						node_id bytea NOT NULL,
						interval_end_time timestamp with time zone NOT NULL,
						data_total double precision NOT NULL,
						data_type integer NOT NULL,
						created_at timestamp with time zone NOT NULL,
						PRIMARY KEY ( id )
					)`,
					`CREATE TABLE IF NOT EXISTS accounting_rollups (
						id bigserial NOT NULL,
						node_id bytea NOT NULL,
						start_time timestamp with time zone NOT NULL,
						put_total bigint NOT NULL,
						get_total bigint NOT NULL,
						get_audit_total bigint NOT NULL,
						get_repair_total bigint NOT NULL,
						put_repair_total bigint NOT NULL,
						at_rest_total double precision NOT NULL,
						PRIMARY KEY ( id )
					)`,
					`CREATE TABLE IF NOT EXISTS accounting_timestamps (
						name text NOT NULL,
						value timestamp with time zone NOT NULL,
						PRIMARY KEY ( name )
					)`,
					`CREATE TABLE IF NOT EXISTS bwagreements (
						serialnum text NOT NULL,
						data bytea NOT NULL,
						storage_node bytea NOT NULL,
						action bigint NOT NULL,
						total bigint NOT NULL,
						created_at timestamp with time zone NOT NULL,
						expires_at timestamp with time zone NOT NULL,
						PRIMARY KEY ( serialnum )
					)`,
					`CREATE TABLE IF NOT EXISTS injuredsegments (
						id bigserial NOT NULL,
						info bytea NOT NULL,
						PRIMARY KEY ( id )
					)`,
					`CREATE TABLE IF NOT EXISTS irreparabledbs (
						segmentpath bytea NOT NULL,
						segmentdetail bytea NOT NULL,
						pieces_lost_count bigint NOT NULL,
						seg_damaged_unix_sec bigint NOT NULL,
						repair_attempt_count bigint NOT NULL,
						PRIMARY KEY ( segmentpath )
					)`,
					`CREATE TABLE IF NOT EXISTS nodes (
						id bytea NOT NULL,
						audit_success_count bigint NOT NULL,
						total_audit_count bigint NOT NULL,
						audit_success_ratio double precision NOT NULL,
						uptime_success_count bigint NOT NULL,
						total_uptime_count bigint NOT NULL,
						uptime_ratio double precision NOT NULL,
						created_at timestamp with time zone NOT NULL,
						updated_at timestamp with time zone NOT NULL,
						PRIMARY KEY ( id )
					)`,
					`CREATE TABLE IF NOT EXISTS overlay_cache_nodes (
						node_id bytea NOT NULL,
						node_type integer NOT NULL,
						address text NOT NULL,
						protocol integer NOT NULL,
						operator_email text NOT NULL,
						operator_wallet text NOT NULL,
						free_bandwidth bigint NOT NULL,
						free_disk bigint NOT NULL,
						latency_90 bigint NOT NULL,
						audit_success_ratio double precision NOT NULL,
						audit_uptime_ratio double precision NOT NULL,
						audit_count bigint NOT NULL,
						audit_success_count bigint NOT NULL,
						uptime_count bigint NOT NULL,
						uptime_success_count bigint NOT NULL,
						PRIMARY KEY ( node_id ),
						UNIQUE ( node_id )
					)`,
					`CREATE TABLE IF NOT EXISTS projects (
						id bytea NOT NULL,
						name text NOT NULL,
						description text NOT NULL,
						created_at timestamp with time zone NOT NULL,
						PRIMARY KEY ( id )
					)`,
					`CREATE TABLE IF NOT EXISTS users (
						id bytea NOT NULL,
						first_name text NOT NULL,
						last_name text NOT NULL,
						email text NOT NULL,
						password_hash bytea NOT NULL,
						status integer NOT NULL,
						created_at timestamp with time zone NOT NULL,
						PRIMARY KEY ( id )
					)`,
					`CREATE TABLE IF NOT EXISTS api_keys (
						id bytea NOT NULL,
						project_id bytea NOT NULL REFERENCES projects( id ) ON DELETE CASCADE,
						key bytea NOT NULL,
						name text NOT NULL,
						created_at timestamp with time zone NOT NULL,
						PRIMARY KEY ( id ),
						UNIQUE ( key ),
						UNIQUE ( name, project_id )
					)`,
					`CREATE TABLE IF NOT EXISTS project_members (
						member_id bytea NOT NULL REFERENCES users( id ) ON DELETE CASCADE,
						project_id bytea NOT NULL REFERENCES projects( id ) ON DELETE CASCADE,
						created_at timestamp with time zone NOT NULL,
						PRIMARY KEY ( member_id, project_id )
					)`,
				},
			},
			{
				// some databases may have already this done, although the version may not match
				Description: "Adjust table naming",
				Version:     1,
				Action: migrate.Func(func(log *zap.Logger, db migrate.DB, tx *sql.Tx) error {
					hasStorageNodeID, err := postgresHasColumn(tx, "bwagreements", "storage_node_id")
					if err != nil {
						return ErrMigrate.Wrap(err)
					}
					if !hasStorageNodeID {
						// - storage_node bytea NOT NULL,
						// + storage_node_id bytea NOT NULL,
						_, err := tx.Exec(`ALTER TABLE bwagreements RENAME COLUMN storage_node TO storage_node_id;`)
						if err != nil {
							return ErrMigrate.Wrap(err)
						}
					}

					hasUplinkID, err := postgresHasColumn(tx, "bwagreements", "uplink_id")
					if err != nil {
						return ErrMigrate.Wrap(err)
					}
					if !hasUplinkID {
						// + uplink_id bytea NOT NULL,
						_, err := tx.Exec(`
							ALTER TABLE bwagreements ADD COLUMN uplink_id BYTEA;
						`)
						if err != nil {
							return ErrMigrate.Wrap(err)
						}

						err = func() error {
							_, err = tx.Exec(`
							DECLARE bwagreements_cursor CURSOR FOR
							SELECT serialnum, data FROM bwagreements
							FOR UPDATE`)
							if err != nil {
								return ErrMigrate.Wrap(err)
							}
							defer func() {
								_, closeErr := tx.Exec(`CLOSE bwagreements_cursor`)
								err = errs.Combine(err, closeErr)
							}()

							for {
								var serialnum, data []byte

								err := tx.QueryRow(`FETCH NEXT FROM bwagreements_cursor`).Scan(&serialnum, &data)
								if err != nil {
									if err == sql.ErrNoRows {
										break
									}
									return ErrMigrate.Wrap(err)
								}

								var rba pb.Order
								if err := proto.Unmarshal(data, &rba); err != nil {
									return ErrMigrate.Wrap(err)
								}

								_, err = tx.Exec(`
									UPDATE bwagreements SET uplink_id = $1
									WHERE CURRENT OF bwagreements_cursor`, rba.PayerAllocation.UplinkId.Bytes())
								if err != nil {
									return ErrMigrate.Wrap(err)
								}
							}
							return nil
						}()
						if err != nil {
							return err
						}

						_, err = tx.Exec(`
							ALTER TABLE bwagreements ALTER COLUMN uplink_id SET NOT NULL;
							ALTER TABLE bwagreements DROP COLUMN data;
						`)
						if err != nil {
							return ErrMigrate.Wrap(err)
						}
					}
					return nil
				}),
			},
			{
				// some databases may have already this done, although the version may not match
				Description: "Remove bucket infos",
				Version:     2,
				Action: migrate.SQL{
					`DROP TABLE IF EXISTS bucket_infos CASCADE`,
				},
			},
			{
				// some databases may have already this done, although the version may not match
				Description: "Add certificates table",
				Version:     3,
				Action: migrate.SQL{
					`CREATE TABLE IF NOT EXISTS certRecords (
						publickey bytea NOT NULL,
						id bytea NOT NULL,
						update_at timestamp with time zone NOT NULL,
						PRIMARY KEY ( id )
					)`,
				},
			},
			{
				// some databases may have already this done, although the version may not match
				Description: "Adjust users table",
				Version:     4,
				Action: migrate.Func(func(log *zap.Logger, db migrate.DB, tx *sql.Tx) error {
					// - email text,
					// + email text NOT NULL,
					emailNullable, err := postgresColumnNullability(tx, "users", "email")
					if err != nil {
						return ErrMigrate.Wrap(err)
					}
					if emailNullable {
						_, err := tx.Exec(`
							ALTER TABLE users ALTER COLUMN email SET NOT NULL;
						`)
						if err != nil {
							return ErrMigrate.Wrap(err)
						}
					}

					// + status integer NOT NULL,
					hasStatus, err := postgresHasColumn(tx, "users", "status")
					if err != nil {
						return ErrMigrate.Wrap(err)
					}
					if !hasStatus {
						_, err := tx.Exec(`
							ALTER TABLE users ADD COLUMN status INTEGER;
							UPDATE users SET status = ` + strconv.Itoa(int(console.Active)) + `;
							ALTER TABLE users ALTER COLUMN status SET NOT NULL;
						`)
						if err != nil {
							return ErrMigrate.Wrap(err)
						}
					}

					// - UNIQUE ( email )
					_, err = tx.Exec(`
						ALTER TABLE users DROP CONSTRAINT IF EXISTS users_email_key;
					`)
					if err != nil {
						return ErrMigrate.Wrap(err)
					}

					return nil
				}),
			},
			{
				Description: "Add wallet column",
				Version:     5,
				Action: migrate.SQL{
					`ALTER TABLE nodes ADD wallet TEXT;
					ALTER TABLE nodes ADD email TEXT;
					UPDATE nodes SET wallet = '';
					UPDATE nodes SET email = '';
					ALTER TABLE nodes ALTER COLUMN wallet SET NOT NULL;
					ALTER TABLE nodes ALTER COLUMN email SET NOT NULL;`,
				},
			},
			{
				Description: "Add bucket usage rollup table",
				Version:     6,
				Action: migrate.SQL{
					`CREATE TABLE bucket_usages (
  						id bytea NOT NULL,
  						bucket_id bytea NOT NULL,
  						rollup_end_time timestamp with time zone NOT NULL,
  						remote_stored_data bigint NOT NULL,
  						inline_stored_data bigint NOT NULL,
  						remote_segments integer NOT NULL,
  						inline_segments integer NOT NULL,
  						objects integer NOT NULL,
  						metadata_size bigint NOT NULL,
  						repair_egress bigint NOT NULL,
  						get_egress bigint NOT NULL,
  						audit_egress bigint NOT NULL,
  						PRIMARY KEY ( id ),
						UNIQUE ( rollup_end_time, bucket_id )
					)`,
				},
			},
			{
				Description: "Add index on bwagreements",
				Version:     7,
				Action: migrate.SQL{
					`CREATE INDEX IF NOT EXISTS bwa_created_at ON bwagreements (created_at)`,
				},
			},
			{
				Description: "Add registration_tokens table",
				Version:     8,
				Action: migrate.SQL{
					`CREATE TABLE registration_tokens (
                         secret bytea NOT NULL,
						 owner_id bytea,
						 project_limit integer NOT NULL,
						 created_at timestamp with time zone NOT NULL,
						 PRIMARY KEY ( secret ),
						 UNIQUE ( owner_id )
					)`,
				},
			},
			{
				Description: "Add new tables for tracking used serials, bandwidth and storage",
				Version:     9,
				Action: migrate.SQL{
					`CREATE TABLE serial_numbers (
						id serial NOT NULL,
						serial_number bytea NOT NULL,
						bucket_id bytea NOT NULL,
						expires_at timestamp NOT NULL,
						PRIMARY KEY ( id )
					)`,
					`CREATE INDEX serial_numbers_expires_at_index ON serial_numbers ( expires_at )`,
					`CREATE UNIQUE INDEX serial_number_index ON serial_numbers ( serial_number )`,
					`CREATE TABLE used_serials (
						serial_number_id integer NOT NULL REFERENCES serial_numbers( id ) ON DELETE CASCADE,
						storage_node_id bytea NOT NULL,
						PRIMARY KEY ( serial_number_id, storage_node_id )
					)`,
					`CREATE TABLE storagenode_bandwidth_rollups (
						storagenode_id bytea NOT NULL,
						interval_start timestamp NOT NULL,
						interval_seconds integer NOT NULL,
						action integer NOT NULL,
						allocated bigint NOT NULL,
						settled bigint NOT NULL,
						PRIMARY KEY ( storagenode_id, interval_start, action )
					)`,
					`CREATE INDEX storagenode_id_interval_start_interval_seconds_index ON storagenode_bandwidth_rollups (
						storagenode_id,
						interval_start,
						interval_seconds
					)`,
					`CREATE TABLE storagenode_storage_rollups (
						storagenode_id bytea NOT NULL,
						interval_start timestamp NOT NULL,
						interval_seconds integer NOT NULL,
						total bigint NOT NULL,
						PRIMARY KEY ( storagenode_id, interval_start )
					)`,
					`CREATE TABLE bucket_bandwidth_rollups (
						bucket_id bytea NOT NULL,
						interval_start timestamp NOT NULL,
						interval_seconds integer NOT NULL,
						action integer NOT NULL,
						inline bigint NOT NULL,
						allocated bigint NOT NULL,
						settled bigint NOT NULL,
						PRIMARY KEY ( bucket_id, interval_start, action )
					)`,
					`CREATE INDEX bucket_id_interval_start_interval_seconds_index ON bucket_bandwidth_rollups (
						bucket_id,
						interval_start,
						interval_seconds
					)`,
					`CREATE TABLE bucket_storage_rollups (
						bucket_id bytea NOT NULL,
						interval_start timestamp NOT NULL,
						interval_seconds integer NOT NULL,
						inline bigint NOT NULL,
						remote bigint NOT NULL,
						PRIMARY KEY ( bucket_id, interval_start )
					)`,
					`ALTER TABLE bucket_usages DROP CONSTRAINT bucket_usages_rollup_end_time_bucket_id_key`,
					`CREATE UNIQUE INDEX bucket_id_rollup_end_time_index ON bucket_usages (
						bucket_id,
						rollup_end_time )`,
				},
			},
			{
				Description: "users first_name to full_name, last_name to short_name",
				Version:     10,
				Action: migrate.SQL{
					`ALTER TABLE users RENAME COLUMN first_name TO full_name;
					ALTER TABLE users ALTER COLUMN last_name DROP NOT NULL;
					ALTER TABLE users RENAME COLUMN last_name TO short_name;`,
				},
			},
			{
				Description: "drops interval seconds from storage_rollups, renames x_storage_rollups to x_storage_tallies, adds fields to bucket_storage_tallies",
				Version:     11,
				Action: migrate.SQL{
					`ALTER TABLE storagenode_storage_rollups RENAME TO storagenode_storage_tallies`,
					`ALTER TABLE bucket_storage_rollups RENAME TO bucket_storage_tallies`,

					`ALTER TABLE storagenode_storage_tallies DROP COLUMN interval_seconds`,
					`ALTER TABLE bucket_storage_tallies DROP COLUMN interval_seconds`,

					`ALTER TABLE bucket_storage_tallies ADD remote_segments_count integer;
					UPDATE bucket_storage_tallies SET remote_segments_count = 0;
					ALTER TABLE bucket_storage_tallies ALTER COLUMN remote_segments_count SET NOT NULL;`,

					`ALTER TABLE bucket_storage_tallies ADD inline_segments_count integer;
					UPDATE bucket_storage_tallies SET inline_segments_count = 0;
					ALTER TABLE bucket_storage_tallies ALTER COLUMN inline_segments_count SET NOT NULL;`,

					`ALTER TABLE bucket_storage_tallies ADD object_count integer;
					UPDATE bucket_storage_tallies SET object_count = 0;
					ALTER TABLE bucket_storage_tallies ALTER COLUMN object_count SET NOT NULL;`,

					`ALTER TABLE bucket_storage_tallies ADD metadata_size bigint;
					UPDATE bucket_storage_tallies SET metadata_size = 0;
					ALTER TABLE bucket_storage_tallies ALTER COLUMN metadata_size SET NOT NULL;`,
				},
			},
			{
				Description: "Merge overlay_cache_nodes into nodes table",
				Version:     12,
				Action: migrate.SQL{
					// Add the new columns to the nodes table
					`ALTER TABLE nodes ADD address TEXT NOT NULL DEFAULT '';
					 ALTER TABLE nodes ADD protocol INTEGER NOT NULL DEFAULT 0;
					 ALTER TABLE nodes ADD type INTEGER NOT NULL DEFAULT 2;
					 ALTER TABLE nodes ADD free_bandwidth BIGINT NOT NULL DEFAULT -1;
					 ALTER TABLE nodes ADD free_disk BIGINT NOT NULL DEFAULT -1;
					 ALTER TABLE nodes ADD latency_90 BIGINT NOT NULL DEFAULT 0;
					 ALTER TABLE nodes ADD last_contact_success TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT 'epoch';
					 ALTER TABLE nodes ADD last_contact_failure TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT 'epoch';`,
					// Copy data from overlay_cache_nodes to nodes
					`UPDATE nodes
					 SET address=overlay.address,
					     protocol=overlay.protocol,
						 type=overlay.node_type,
						 free_bandwidth=overlay.free_bandwidth,
						 free_disk=overlay.free_disk,
						 latency_90=overlay.latency_90
					 FROM (SELECT node_id, node_type, address, protocol, free_bandwidth, free_disk, latency_90
						   FROM overlay_cache_nodes) AS overlay
					 WHERE nodes.id=overlay.node_id;`,
					// Delete the overlay cache_nodes table
					`DROP TABLE overlay_cache_nodes CASCADE;`,
				},
			},
			{
				Description: "Change bucket_id to bucket_name and project_id",
				Version:     13,
				Action: migrate.SQL{
					// Modify columns: bucket_id --> bucket_name + project_id for table bucket_storage_tallies
					`ALTER TABLE bucket_storage_tallies ADD project_id bytea;`,
					`UPDATE bucket_storage_tallies SET project_id=SUBSTRING(bucket_id FROM 1 FOR 16);`,
					`ALTER TABLE bucket_storage_tallies ALTER COLUMN project_id SET NOT NULL;`,
					`ALTER TABLE bucket_storage_tallies RENAME COLUMN bucket_id TO bucket_name;`,
					`UPDATE bucket_storage_tallies SET bucket_name=SUBSTRING(bucket_name from 18);`,

					// Update the primary key for bucket_storage_tallies
					`ALTER TABLE bucket_storage_tallies DROP CONSTRAINT bucket_storage_rollups_pkey;`,
					`ALTER TABLE bucket_storage_tallies ADD CONSTRAINT bucket_storage_tallies_pk PRIMARY KEY (bucket_name, project_id, interval_start);`,

					// Modify columns: bucket_id --> bucket_name + project_id for table bucket_bandwidth_rollups
					`ALTER TABLE bucket_bandwidth_rollups ADD project_id bytea;`,
					`UPDATE bucket_bandwidth_rollups SET project_id=SUBSTRING(bucket_id FROM 1 FOR 16);`,
					`ALTER TABLE bucket_bandwidth_rollups ALTER COLUMN project_id SET NOT NULL;`,
					`ALTER TABLE bucket_bandwidth_rollups RENAME COLUMN bucket_id TO bucket_name;`,
					`UPDATE bucket_bandwidth_rollups SET bucket_name=SUBSTRING(bucket_name from 18);`,

					// Update index for bucket_bandwidth_rollups
					`DROP INDEX IF EXISTS bucket_id_interval_start_interval_seconds_index;`,
					`CREATE INDEX bucket_name_project_id_interval_start_interval_seconds ON bucket_bandwidth_rollups (
						bucket_name,
						project_id,
						interval_start,
						interval_seconds
					);`,

					// Update the primary key for bucket_bandwidth_rollups
					`ALTER TABLE bucket_bandwidth_rollups DROP CONSTRAINT bucket_bandwidth_rollups_pkey;`,
					`ALTER TABLE bucket_bandwidth_rollups ADD CONSTRAINT bucket_bandwidth_rollups_pk PRIMARY KEY (bucket_name, project_id, interval_start, action);`,
				},
			},
			{
				Description: "Add new Columns to store version information",
				Version:     14,
				Action: migrate.SQL{
					`ALTER TABLE nodes ADD major bigint NOT NULL DEFAULT 0;
					ALTER TABLE nodes ADD minor bigint NOT NULL DEFAULT 1;
					ALTER TABLE nodes ADD patch bigint NOT NULL DEFAULT 0;
					ALTER TABLE nodes ADD hash TEXT NOT NULL DEFAULT '';
					ALTER TABLE nodes ADD timestamp TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT 'epoch';
					ALTER TABLE nodes ADD release bool NOT NULL DEFAULT FALSE;`,
				},
			},
			{
				Description: "Default Node Type should be invalid",
				Version:     15,
				Action: migrate.SQL{
					`ALTER TABLE nodes ALTER COLUMN type SET DEFAULT 0;`,
				},
			},
			{
				Description: "Drops storagenode_storage_tally table, Renames accounting_raws to storagenode_storage_tally, and Drops data_type column",
				Version:     16,
				Action: migrate.SQL{
					`DROP TABLE storagenode_storage_tallies CASCADE`,
					`ALTER TABLE accounting_raws RENAME TO storagenode_storage_tallies`,
					`ALTER TABLE storagenode_storage_tallies DROP COLUMN data_type`,
				},
			},
		},
	}
}

func postgresHasColumn(tx *sql.Tx, table, column string) (bool, error) {
	var columnName string
	err := tx.QueryRow(`
		SELECT column_name FROM information_schema.COLUMNS
			WHERE table_schema = CURRENT_SCHEMA
				AND table_name = $1
				AND column_name = $2
		`, table, column).Scan(&columnName)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, ErrMigrate.Wrap(err)
	}

	return columnName == column, nil
}

func postgresColumnNullability(tx *sql.Tx, table, column string) (bool, error) {
	var nullability string
	err := tx.QueryRow(`
		SELECT is_nullable FROM information_schema.COLUMNS
			WHERE table_schema = CURRENT_SCHEMA
				AND table_name = $1
				AND column_name = $2
		`, table, column).Scan(&nullability)
	if err != nil {
		return false, ErrMigrate.Wrap(err)
	}
	return nullability == "YES", nil
}
