package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (db *SQLDB) InsertVolume(data Volume) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}

	defer tx.Rollback()

	var resourceVersion []byte

	columns := []string{"worker_name", "ttl", "handle"}
	params := []interface{}{data.WorkerName, data.TTL, data.Handle}
	values := []string{"$1", "$2", "$3"}

	if data.TTL == 0 {
		columns = append(columns, "expires_at")
		values = append(values, "NULL")
	} else {
		columns = append(columns, "expires_at")
		params = append(params, fmt.Sprintf("%d second", int(data.TTL.Seconds())))
		values = append(values, fmt.Sprintf("NOW() + $%d::INTERVAL", len(params)))
	}

	if data.Identifier.ResourceCache != nil {
		resourceVersion, err = json.Marshal(data.Identifier.ResourceCache.ResourceVersion)
		if err != nil {
			return err
		}

		columns = append(columns, "resource_version")
		params = append(params, resourceVersion)
		values = append(values, fmt.Sprintf("$%d", len(params)))

		columns = append(columns, "resource_hash")
		params = append(params, data.Identifier.ResourceCache.ResourceHash)
		values = append(values, fmt.Sprintf("$%d", len(params)))
	} else if data.Identifier.COW != nil {
		columns = append(columns, "original_volume_handle")
		params = append(params, data.Identifier.COW.ParentVolumeHandle)
		values = append(values, fmt.Sprintf("$%d", len(params)))
	} else if data.Identifier.Output != nil {
		columns = append(columns, "output_name")
		params = append(params, data.Identifier.Output.Name)
		values = append(values, fmt.Sprintf("$%d", len(params)))
	}

	_, err = tx.Exec(
		fmt.Sprintf(
			`
				INSERT INTO volumes(
					%s
				) VALUES (
					%s
				)
			`,
			strings.Join(columns, ", "),
			strings.Join(values, ", "),
		), params...)
	if err != nil {
		if strings.Contains(err.Error(), `duplicate key value violates unique constraint "volumes_worker_name_handle_key"`) {
			return nil
		}

		return err
	}

	return tx.Commit()
}

func (db *SQLDB) ReapVolume(handle string) error {
	_, err := db.conn.Exec(`
		DELETE FROM volumes
		WHERE handle = $1
	`, handle)
	return err
}

func (db *SQLDB) GetVolumes() ([]SavedVolume, error) {
	err := db.expireVolumes()
	if err != nil {
		return nil, err
	}

	rows, err := db.conn.Query(`
		SELECT
			worker_name,
			ttl,
			EXTRACT(epoch FROM expires_at - NOW()),
			handle,
			resource_version,
			resource_hash,
			id,
			original_volume_handle,
			output_name
		FROM volumes
	`)
	if err != nil {
		return nil, err
	}

	volumes, err := scanVolumes(rows)
	return volumes, err
}

func (db *SQLDB) GetVolumesForOneOffBuildImageResources() ([]SavedVolume, error) {
	err := db.expireVolumes()
	if err != nil {
		return nil, err
	}

	rows, err := db.conn.Query(`
		SELECT DISTINCT
			v.worker_name,
			v.ttl,
			EXTRACT(epoch FROM v.expires_at - NOW()),
			v.handle,
			v.resource_version,
			v.resource_hash,
			v.id,
			v.original_volume_handle,
			v.output_name
		FROM volumes v
			INNER JOIN image_resource_versions i
				ON i.version = v.resource_version
				AND i.resource_hash = v.resource_hash
			INNER JOIN builds b
				ON b.id = i.build_id
		WHERE b.job_id IS NULL
	`)
	if err != nil {
		return nil, err
	}

	volumes, err := scanVolumes(rows)
	return volumes, err
}

func (db *SQLDB) SetVolumeTTL(handle string, ttl time.Duration) error {
	if ttl == 0 {
		_, err := db.conn.Exec(`
			UPDATE volumes
			SET expires_at = null, ttl = 0
			WHERE handle = $1
		`, handle)

		return err
	}

	interval := fmt.Sprintf("%d second", int(ttl.Seconds()))

	_, err := db.conn.Exec(`
		UPDATE volumes
		SET expires_at = NOW() + $1::INTERVAL,
		ttl = $2
		WHERE handle = $3
	`, interval, ttl, handle)

	return err
}

func (db *SQLDB) GetVolumeTTL(handle string) (time.Duration, bool, error) {
	var ttl time.Duration

	err := db.conn.QueryRow(`
		SELECT ttl
		FROM volumes
		WHERE handle = $1
	`, handle).Scan(&ttl)
	if err == sql.ErrNoRows {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}

	return ttl, true, nil
}

func (db *SQLDB) getVolume(originalVolumeHandle string) (SavedVolume, error) {
	err := db.expireVolumes()
	if err != nil {
		return SavedVolume{}, err
	}

	rows, err := db.conn.Query(`
		SELECT
			worker_name,
			ttl,
			EXTRACT(epoch FROM expires_at - NOW()),
			handle,
			resource_version,
			resource_hash,
			id,
			original_volume_handle
			output_name
		FROM volumes
		WHERE handle = $1
	`, originalVolumeHandle)
	if err != nil {
		return SavedVolume{}, err
	}

	volumes, err := scanVolumes(rows)
	if err != nil {
		return SavedVolume{}, err
	}

	switch len(volumes) {
	case 0:
		return SavedVolume{}, errors.New(fmt.Sprintf("unable to find volume handle %s", originalVolumeHandle))
	case 1:
		return volumes[0], nil
	default:
		return SavedVolume{}, errors.New(fmt.Sprintf("%d volumes found for handle %s", len(volumes), originalVolumeHandle))
	}
}

func (db *SQLDB) expireVolumes() error {
	_, err := db.conn.Exec(`
		DELETE FROM volumes
		WHERE expires_at IS NOT NULL
		AND expires_at < NOW()
	`)
	return err
}

func scanVolumes(rows *sql.Rows) ([]SavedVolume, error) {
	defer rows.Close()

	volumes := []SavedVolume{}

	for rows.Next() {
		var volume SavedVolume
		var ttlSeconds *float64
		var versionJSON sql.NullString
		var resourceHash sql.NullString
		var originalVolumeHandle sql.NullString
		var outputName sql.NullString

		err := rows.Scan(&volume.WorkerName, &volume.TTL, &ttlSeconds, &volume.Handle, &versionJSON, &resourceHash, &volume.ID, &originalVolumeHandle, &outputName)
		if err != nil {
			return nil, err
		}

		if ttlSeconds != nil {
			volume.ExpiresIn = time.Duration(*ttlSeconds) * time.Second
		}

		if versionJSON.Valid && resourceHash.Valid {
			var cacheID ResourceCacheIdentifier

			err = json.Unmarshal([]byte(versionJSON.String), &cacheID.ResourceVersion)
			if err != nil {
				return nil, err
			}

			cacheID.ResourceHash = resourceHash.String

			volume.Volume.Identifier.ResourceCache = &cacheID
		} else if originalVolumeHandle.Valid {
			volume.Volume.Identifier.COW = &COWIdentifier{
				ParentVolumeHandle: originalVolumeHandle.String,
			}
		} else if outputName.Valid {
			volume.Volume.Identifier.Output = &OutputIdentifier{
				Name: outputName.String,
			}
		}

		volumes = append(volumes, volume)
	}

	return volumes, nil
}
