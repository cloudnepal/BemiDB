package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/linkedin/goavro"
	"github.com/xitongsys/parquet-go/reader"
	"github.com/xitongsys/parquet-go/schema"
	"github.com/xitongsys/parquet-go/source"
	"github.com/xitongsys/parquet-go/writer"
)

type StorageBase struct {
	config *Config
}

func (storage *StorageBase) WriteParquetFile(fileWriter source.ParquetFile, pgSchemaColumns []PgSchemaColumn, loadRows func() [][]string) (recordCount int64, err error) {
	defer fileWriter.Close()

	stringSchemaFields := make([]string, len(pgSchemaColumns))
	for i, pgSchemaColumn := range pgSchemaColumns {
		stringSchemaFields[i] = pgSchemaColumn.ToStringParquetSchemaField()
		LogDebug(storage.config, "Parquet schema field:", stringSchemaFields[i])
	}

	parquetWriter, err := writer.NewCSVWriter(stringSchemaFields, fileWriter, PARQUET_PARALLEL_NUMBER)
	if err != nil {
		return 0, fmt.Errorf("Failed to create Parquet writer: %v", err)
	}

	parquetWriter.RowGroupSize = PARQUET_ROW_GROUP_SIZE
	parquetWriter.CompressionType = PARQUET_COMPRESSION_TYPE

	rows := loadRows()
	for len(rows) > 0 {
		for _, row := range rows {
			LogDebug(storage.config, "Parquet row:", row)
			rowWithPointers := make([]*string, len(row))
			for i := 0; i < len(row); i++ {
				rowWithPointers[i] = pgSchemaColumns[i].FormatParquetValue(row[i])
				// LogDebug(storage.config, "Pg schema column:", pgSchemaColumns[i].ToStringParquetSchemaField())
				// if rowWithPointers[i] == nil {
				// 	LogDebug(storage.config, "Parquet row value: nil")
				// } else {
				// 	LogDebug(storage.config, "Parquet row value:", *rowWithPointers[i])
				// }
			}
			if err = parquetWriter.WriteString(rowWithPointers); err != nil {
				return 0, fmt.Errorf("Write error: %v", err)
			}
			recordCount++
		}
		rows = loadRows()
	}

	if err := parquetWriter.WriteStop(); err != nil {
		return 0, fmt.Errorf("Failed to stop Parquet writer: %v", err)
	}

	return recordCount, nil
}

func (storage *StorageBase) ReadParquetStats(fileReader source.ParquetFile) (parquetFileStats ParquetFileStats, err error) {
	defer fileReader.Close()

	pr, err := reader.NewParquetReader(fileReader, nil, 1)
	if err != nil {
		return ParquetFileStats{}, fmt.Errorf("Failed to create Parquet reader: %v", err)
	}
	defer pr.ReadStop()

	parquetStats := ParquetFileStats{
		ColumnSizes:     make(map[int]int64),
		ValueCounts:     make(map[int]int64),
		NullValueCounts: make(map[int]int64),
		LowerBounds:     make(map[int][]byte),
		UpperBounds:     make(map[int][]byte),
		SplitOffsets:    []int64{},
	}

	fieldIDMap := storage.buildFieldIDMap(pr.SchemaHandler)

	for _, rowGroup := range pr.Footer.RowGroups {
		if rowGroup.FileOffset != nil {
			parquetStats.SplitOffsets = append(parquetStats.SplitOffsets, *rowGroup.FileOffset)
		}

		for _, columnChunk := range rowGroup.Columns {
			columnMetaData := columnChunk.MetaData
			columnPath := columnMetaData.PathInSchema
			columnName := strings.Join(columnPath, ".")
			fieldID, ok := fieldIDMap[columnName]
			if !ok {
				continue
			}
			parquetStats.ColumnSizes[fieldID] += columnMetaData.TotalCompressedSize
			parquetStats.ValueCounts[fieldID] += int64(columnMetaData.NumValues)

			if columnMetaData.Statistics != nil {
				if columnMetaData.Statistics.NullCount != nil {
					parquetStats.NullValueCounts[fieldID] += *columnMetaData.Statistics.NullCount
				}

				minValue := columnMetaData.Statistics.Min
				maxValue := columnMetaData.Statistics.Max

				if parquetStats.LowerBounds[fieldID] == nil || bytes.Compare(parquetStats.LowerBounds[fieldID], minValue) > 0 {
					parquetStats.LowerBounds[fieldID] = minValue
				}
				if parquetStats.UpperBounds[fieldID] == nil || bytes.Compare(parquetStats.UpperBounds[fieldID], maxValue) < 0 {
					parquetStats.UpperBounds[fieldID] = maxValue
				}
			}
		}
	}

	// Todo: convert lower/upper bytes to BigEndianBytes?

	return parquetStats, nil
}

func (storage *StorageBase) WriteManifestFile(fileSystemPrefix string, filePath string, parquetFile ParquetFile) (manifestFile ManifestFile, err error) {
	snapshotId := time.Now().UnixNano()
	codec, err := goavro.NewCodec(MANIFEST_SCHEMA)
	if err != nil {
		return ManifestFile{}, fmt.Errorf("Failed to create Avro codec: %v", err)
	}

	columnSizesArr := []interface{}{}
	for fieldID, size := range parquetFile.Stats.ColumnSizes {
		columnSizesArr = append(columnSizesArr, map[string]interface{}{
			"key":   fieldID,
			"value": size,
		})
	}

	valueCountsArr := []interface{}{}
	for fieldID, count := range parquetFile.Stats.ValueCounts {
		valueCountsArr = append(valueCountsArr, map[string]interface{}{
			"key":   fieldID,
			"value": count,
		})
	}

	nullValueCountsArr := []interface{}{}
	for fieldID, count := range parquetFile.Stats.NullValueCounts {
		nullValueCountsArr = append(nullValueCountsArr, map[string]interface{}{
			"key":   fieldID,
			"value": count,
		})
	}

	lowerBoundsArr := []interface{}{}
	for fieldID, value := range parquetFile.Stats.LowerBounds {
		lowerBoundsArr = append(lowerBoundsArr, map[string]interface{}{
			"key":   fieldID,
			"value": value,
		})
	}

	upperBoundsArr := []interface{}{}
	for fieldID, value := range parquetFile.Stats.UpperBounds {
		upperBoundsArr = append(upperBoundsArr, map[string]interface{}{
			"key":   fieldID,
			"value": value,
		})
	}

	dataFile := map[string]interface{}{
		"content":     0, // 0: DATA, 1: POSITION DELETES, 2: EQUALITY DELETES
		"file_path":   fileSystemPrefix + parquetFile.Path,
		"file_format": "PARQUET",
		// TODO: figure out "partition": ...
		"record_count":       parquetFile.RecordCount,
		"file_size_in_bytes": parquetFile.Size,
		"column_sizes": map[string]interface{}{
			"array": columnSizesArr,
		},
		"value_counts": map[string]interface{}{
			"array": valueCountsArr,
		},
		"null_value_counts": map[string]interface{}{
			"array": nullValueCountsArr,
		},
		"nan_value_counts": map[string]interface{}{
			"array": []interface{}{},
		},
		"lower_bounds": map[string]interface{}{
			"array": lowerBoundsArr,
		},
		"upper_bounds": map[string]interface{}{
			"array": upperBoundsArr,
		},
		"key_metadata": nil,
		"split_offsets": map[string]interface{}{
			"array": parquetFile.Stats.SplitOffsets,
		},
		"equality_ids":  nil,
		"sort_order_id": nil,
	}

	manifestEntry := map[string]interface{}{
		"status":               1, // 0: EXISTING 1: ADDED 2: DELETED
		"snapshot_id":          map[string]interface{}{"long": snapshotId},
		"sequence_number":      nil,
		"file_sequence_number": nil,
		"data_file":            dataFile,
	}

	avroFile, err := os.Create(filePath)
	if err != nil {
		return ManifestFile{}, fmt.Errorf("Failed to create manifest file: %v", err)
	}
	defer avroFile.Close()

	ocfWriter, err := goavro.NewOCFWriter(goavro.OCFConfig{
		W:      avroFile,
		Codec:  codec,
		Schema: MANIFEST_SCHEMA,
	})
	if err != nil {
		return ManifestFile{}, fmt.Errorf("Failed to create Avro OCF writer: %v", err)
	}

	err = ocfWriter.Append([]interface{}{manifestEntry})
	if err != nil {
		return ManifestFile{}, fmt.Errorf("Failed to write to manifest file: %v", err)
	}

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return ManifestFile{}, fmt.Errorf("Failed to get manifest file info: %v", err)
	}
	fileSize := fileInfo.Size()

	return ManifestFile{
		SnapshotId: snapshotId,
		Path:       filePath,
		Size:       fileSize,
	}, nil
}

func (storage *StorageBase) WriteManifestListFile(fileSystemPrefix string, filePath string, parquetFile ParquetFile, manifestFile ManifestFile) (err error) {
	codec, err := goavro.NewCodec(MANIFEST_LIST_SCHEMA)
	if err != nil {
		return fmt.Errorf("Failed to create Avro codec for manifest list: %v", err)
	}

	manifestListRecord := map[string]interface{}{
		"added_files_count":    1,
		"added_rows_count":     parquetFile.RecordCount,
		"added_snapshot_id":    manifestFile.SnapshotId,
		"content":              0,
		"deleted_files_count":  0,
		"deleted_rows_count":   0,
		"existing_files_count": 0,
		"existing_rows_count":  0,
		"key_metadata":         nil,
		"manifest_length":      manifestFile.Size,
		"manifest_path":        fileSystemPrefix + manifestFile.Path,
		"min_sequence_number":  1,
		"partition_spec_id":    0,
		"partitions":           map[string]interface{}{"array": []string{}},
		"sequence_number":      1,
	}

	avroFile, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("Failed to create manifest list file: %v", err)
	}
	defer avroFile.Close()

	ocfWriter, err := goavro.NewOCFWriter(goavro.OCFConfig{
		W:      avroFile,
		Codec:  codec,
		Schema: MANIFEST_LIST_SCHEMA,
	})
	if err != nil {
		return fmt.Errorf("Failed to create OCF writer for manifest list: %v", err)
	}

	err = ocfWriter.Append([]interface{}{manifestListRecord})
	if err != nil {
		return fmt.Errorf("Failed to write manifest list record: %v", err)
	}

	return nil
}

func (storage *StorageBase) WriteMetadataFile(fileSystemPrefix string, filePath string, pgSchemaColumns []PgSchemaColumn, parquetFile ParquetFile, manifestFile ManifestFile, manifestListFile ManifestListFile) (err error) {
	tableUuid := uuid.New().String()
	lastColumnID := 3
	currentTimestampMs := time.Now().UnixNano() / int64(time.Millisecond)

	icebergSchemaFields := make([]interface{}, len(pgSchemaColumns))
	for i, pgSchemaColumn := range pgSchemaColumns {
		icebergSchemaFields[i] = pgSchemaColumn.ToMapIcebergSchemaField()
	}

	metadata := map[string]interface{}{
		"format-version":       2,
		"table-uuid":           tableUuid,
		"location":             fileSystemPrefix + filePath,
		"last-sequence-number": 1,
		"last-updated-ms":      currentTimestampMs,
		"last-column-id":       lastColumnID,
		"schemas": []interface{}{
			map[string]interface{}{
				"type":                 "struct",
				"schema-id":            0,
				"fields":               icebergSchemaFields,
				"identifier-field-ids": []interface{}{},
			},
		},
		"current-schema-id": 0,
		"partition-specs": []interface{}{
			map[string]interface{}{
				"spec-id": 0,
				"fields":  []interface{}{},
			},
		},
		"default-spec-id":       0,
		"default-sort-order-id": 0,
		"last-partition-id":     999, // Assuming no partitions; set to a placeholder
		"properties":            map[string]string{},
		"current-snapshot-id":   manifestFile.SnapshotId,
		"refs": map[string]interface{}{
			"main": map[string]interface{}{
				"snapshot-id": manifestFile.SnapshotId,
				"type":        "branch",
			},
		},
		"snapshots": []interface{}{
			map[string]interface{}{
				"schema-id":       0,
				"snapshot-id":     manifestFile.SnapshotId,
				"sequence-number": 1,
				"timestamp-ms":    currentTimestampMs,
				"manifest-list":   fileSystemPrefix + manifestListFile.Path,
				"summary": map[string]interface{}{
					"added-data-files":       "1",
					"added-files-size":       strconv.FormatInt(parquetFile.Size, 10),
					"added-records":          strconv.FormatInt(parquetFile.RecordCount, 10),
					"operation":              "append",
					"total-data-files":       "1",
					"total-delete-files":     "0",
					"total-equality-deletes": "0",
					"total-files-size":       strconv.FormatInt(parquetFile.Size, 10),
					"total-position-deletes": "0",
					"total-records":          strconv.FormatInt(parquetFile.RecordCount, 10),
				},
			},
		},
		"snapshot-log": []interface{}{
			map[string]interface{}{
				"snapshot-id":  manifestFile.SnapshotId,
				"timestamp-ms": currentTimestampMs,
			},
		},
		"metadata-log": []interface{}{},
		"sort-orders": []interface{}{
			map[string]interface{}{
				"order-id": 0,
				"fields":   []interface{}{},
			},
		},
	}

	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("Failed to create metadata file: %v", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(metadata)
	if err != nil {
		return fmt.Errorf("Failed to write metadata to file: %v", err)
	}

	return nil
}

func (storage *StorageBase) WriteVersionHintFile(filePath string, metadataFile MetadataFile) (err error) {
	versionHintFile, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("Failed to create version hint file: %v", err)
	}
	defer versionHintFile.Close()

	_, err = versionHintFile.WriteString(fmt.Sprintf("%d", metadataFile.Version))
	if err != nil {
		return fmt.Errorf("Failed to write to version hint file: %v", err)
	}

	return nil
}

func (storage *StorageBase) buildFieldIDMap(schemaHandler *schema.SchemaHandler) map[string]int {
	fieldIDMap := make(map[string]int)
	for _, schema := range schemaHandler.SchemaElements {
		if schema.FieldID != nil {
			fieldIDMap[schema.Name] = int(*schema.FieldID)
		}
	}
	return fieldIDMap
}
