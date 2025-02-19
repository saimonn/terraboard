package db

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/camptocamp/terraboard/types"
	"github.com/hashicorp/terraform/terraform"

	"github.com/jinzhu/gorm"
	// Use postgres as a DB backend
	_ "github.com/jinzhu/gorm/dialects/postgres"
)

// Database is a wrapping structure to *gorm.DB
type Database struct {
	*gorm.DB
}

var pageSize = 20

// Init setups up the Database and a pointer to it
func Init(host, user, dbname, password, logLevel string) *Database {
	var err error
	connString := fmt.Sprintf("host=%s user=%s dbname=%s sslmode=disable password=%s", host, user, dbname, password)
	db, err := gorm.Open("postgres", connString)
	if err != nil {
		log.Fatal(err)
	}

	log.Infof("Automigrate")
	db.AutoMigrate(&types.Version{}, &types.State{}, &types.Module{}, &types.Resource{}, &types.Attribute{})

	if logLevel == "debug" {
		db.LogMode(true)
	}
	return &Database{db}
}

func (db *Database) stateS3toDB(state *terraform.State, path string, versionID string) (st types.State) {
	var version types.Version
	db.First(&version, types.Version{VersionID: versionID})
	st = types.State{
		Path:      path,
		Version:   version,
		TFVersion: state.TFVersion,
		Serial:    state.Serial,
	}

	for _, m := range state.Modules {
		mod := types.Module{
			Path: strings.Join(m.Path, "/"),
		}
		for n, r := range m.Resources {
			res := types.Resource{
				Type: r.Type,
				Name: n,
			}

			for k, v := range r.Primary.Attributes {
				if !isASCII(v) {
					log.WithFields(log.Fields{
						"key":          k,
						"value_base64": base64.StdEncoding.EncodeToString([]byte(v)),
					}).Info("Attribute has non-ASCII value, skipping")
					continue
				}
				res.Attributes = append(res.Attributes, types.Attribute{
					Key:   k,
					Value: v,
				})
			}

			mod.Resources = append(mod.Resources, res)
		}
		st.Modules = append(st.Modules, mod)
	}
	return
}

func isASCII(s string) bool {
	for _, c := range s {
		if c > 127 {
			return false
		}
	}
	return true
}

// InsertState inserts a Terraform State in the Database
func (db *Database) InsertState(path string, versionID string, state *terraform.State) error {
	st := db.stateS3toDB(state, path, versionID)
	db.Create(&st)
	return nil
}

// InsertVersion inserts an AWS S3 Version in the Database
func (db *Database) InsertVersion(version *s3.ObjectVersion) error {
	var v types.Version
	db.FirstOrCreate(&v, types.Version{
		VersionID:    *version.VersionId,
		LastModified: *version.LastModified,
	})
	return nil
}

// GetState retrieves a State from the database by its path and versionID
func (db *Database) GetState(path, versionID string) (state types.State) {
	db.Joins("JOIN versions on states.version_id=versions.id").
		Preload("Version").Preload("Modules").Preload("Modules.Resources").Preload("Modules.Resources.Attributes").
		Find(&state, "states.path = ? AND versions.version_id = ?", path, versionID)
	return
}

// GetStateActivity returns a slice of StateStat from the Database
// for a given State path representing the State activity over time (Versions)
func (db *Database) GetStateActivity(path string) (states []types.StateStat) {
	sql := "SELECT t.path, t.serial, t.tf_version, t.version_id, t.last_modified, count(resources.*) as resource_count" +
		" FROM (SELECT states.id, states.path, states.serial, states.tf_version, versions.version_id, versions.last_modified FROM states JOIN versions ON versions.id = states.version_id WHERE states.path = ? ORDER BY states.path, versions.last_modified ASC) t" +
		" JOIN modules ON modules.state_id = t.id" +
		" JOIN resources ON resources.module_id = modules.id" +
		" GROUP BY t.path, t.serial, t.tf_version, t.version_id, t.last_modified" +
		" ORDER BY last_modified ASC"

	db.Raw(sql, path).Find(&states)
	return
}

// KnownVersions returns a slice of all known Versions in the Database
func (db *Database) KnownVersions() (versions []string) {
	// TODO: err
	rows, _ := db.Table("versions").Select("DISTINCT version_id").Rows()
	defer rows.Close()
	for rows.Next() {
		var version string
		rows.Scan(&version) // TODO: err
		versions = append(versions, version)
	}
	return
}

// SearchAttribute returns a slice of SearchResult given a query
// The query might contain parameters 'type', 'name', 'key', 'value' and 'tf_version'
// SearchAttribute also returns paging information: the page number and the total results
func (db *Database) SearchAttribute(query url.Values) (results []types.SearchResult, page int, total int) {
	log.WithFields(log.Fields{
		"query": query,
	}).Info("Searching for attribute with query")

	targetVersion := string(query.Get("versionid"))

	sqlQuery := ""
	if targetVersion == "" {
		sqlQuery += " FROM (SELECT states.path, max(states.serial) as mx FROM states GROUP BY states.path) t" +
			" JOIN states ON t.path = states.path AND t.mx = states.serial"
	} else {
		sqlQuery += " FROM states"
	}

	sqlQuery += " JOIN modules ON states.id = modules.state_id" +
		" JOIN resources ON modules.id = resources.module_id" +
		" JOIN attributes ON resources.id = attributes.resource_id"

	var where []string
	var params []interface{}
	if targetVersion != "" && targetVersion != "*" {
		// filter by version unless we want all (*) or most recent ("")
		where = append(where, "states.version_id = ?")
		params = append(params, targetVersion)
	}

	if v := string(query.Get("type")); v != "" {
		where = append(where, "resources.type LIKE ?")
		params = append(params, fmt.Sprintf("%%%s%%", v))
	}

	if v := string(query.Get("name")); v != "" {
		where = append(where, "resources.name LIKE ?")
		params = append(params, fmt.Sprintf("%%%s%%", v))
	}

	if v := string(query.Get("key")); v != "" {
		where = append(where, "attributes.key LIKE ?")
		params = append(params, fmt.Sprintf("%%%s%%", v))
	}

	if v := string(query.Get("value")); v != "" {
		where = append(where, "attributes.value LIKE ?")
		params = append(params, fmt.Sprintf("%%%s%%", v))
	}

	if v := query.Get("tf_version"); string(v) != "" {
		where = append(where, fmt.Sprintf("states.tf_version LIKE '%s'", fmt.Sprintf("%%%s%%", v)))
	}

	if len(where) > 0 {
		sqlQuery += " WHERE " + strings.Join(where, " AND ")
	}

	// Count everything
	row := db.Raw("SELECT count(*)"+sqlQuery, params...).Row()
	row.Scan(&total)

	// Now get results
	// gorm doesn't support subqueries...
	sql := "SELECT states.path, states.version_id, states.tf_version, states.serial, modules.path as module_path, resources.type, resources.name, attributes.key, attributes.value" +
		sqlQuery +
		" ORDER BY states.path, states.serial, modules.path, resources.type, resources.name, attributes.key" +
		" LIMIT ?"

	params = append(params, pageSize)

	if v := string(query.Get("page")); v != "" {
		page, _ = strconv.Atoi(v) // TODO: err
		o := (page - 1) * pageSize
		sql += " OFFSET ?"
		params = append(params, o)
	} else {
		page = 1
	}

	db.Raw(sql, params...).Find(&results)

	return
}

// ListStatesVersions returns a map of Version IDs to a slice of State paths
// from the Database
func (db *Database) ListStatesVersions() (statesVersions map[string][]string) {
	rows, _ := db.Table("states").
		Joins("JOIN versions ON versions.id = states.version_id").
		Select("states.path, versions.version_id").Rows()
	defer rows.Close()
	statesVersions = make(map[string][]string)
	for rows.Next() {
		var path string
		var versionID string
		rows.Scan(&path, &versionID)
		statesVersions[versionID] = append(statesVersions[versionID], path)
	}
	return
}

// ListStates returns a slice of all State paths from the Database
func (db *Database) ListStates() (states []string) {
	rows, _ := db.Table("states").Select("DISTINCT path").Rows()
	defer rows.Close()
	for rows.Next() {
		var state string
		rows.Scan(&state)
		states = append(states, state)
	}
	return
}

// ListTerraformVersionsWithCount returns a slice of maps of Terraform versions
// mapped to the count of most recent State paths using them.
// ListTerraformVersionsWithCount also takes a query with possible parameter 'orderBy'
// to sort results. Default sorting is by descending version number.
func (db *Database) ListTerraformVersionsWithCount(query url.Values) (results []map[string]string, err error) {
	orderBy := string(query.Get("orderBy"))
	sql := "SELECT t.tf_version, COUNT(*)" +
		" FROM (SELECT DISTINCT ON(states.path) states.id, states.path, states.serial, states.tf_version, versions.version_id, versions.last_modified" +
		" FROM states JOIN versions ON versions.id = states.version_id ORDER BY states.path, versions.last_modified DESC) t" +
		" GROUP BY t.tf_version ORDER BY "

	if orderBy == "version" {
		sql += "string_to_array(t.tf_version, '.')::int[] DESC"
	} else {
		sql += "count DESC"
	}

	rows, err := db.Raw(sql).Rows()
	defer rows.Close()
	if err != nil {
		return results, err
	}

	for rows.Next() {
		var name string
		var count string
		r := make(map[string]string)
		rows.Scan(&name, &count)
		r["name"] = name
		r["count"] = count
		results = append(results, r)
	}
	return
}

// ListStateStats returns a slice of StateStat, along with paging information
func (db *Database) ListStateStats(query url.Values) (states []types.StateStat, page int, total int) {
	row := db.Table("states").Select("count(DISTINCT path)").Row()
	row.Scan(&total)

	offset := 0
	page = 1
	if v := string(query.Get("page")); v != "" {
		page, _ = strconv.Atoi(v) // TODO: err
		offset = (page - 1) * pageSize
	}

	sql := "SELECT t.path, t.serial, t.tf_version, t.version_id, t.last_modified, count(resources.*) as resource_count" +
		" FROM (SELECT DISTINCT ON(states.path) states.id, states.path, states.serial, states.tf_version, versions.version_id, versions.last_modified FROM states JOIN versions ON versions.id = states.version_id ORDER BY states.path, versions.last_modified DESC) t" +
		" JOIN modules ON modules.state_id = t.id" +
		" JOIN resources ON resources.module_id = modules.id" +
		" GROUP BY t.path, t.serial, t.tf_version, t.version_id, t.last_modified" +
		" ORDER BY last_modified DESC" +
		" LIMIT 20" +
		" OFFSET ?"

	db.Raw(sql, offset).Find(&states)
	return
}

// listField is a wrapper utility method to list distinct values in Database tables.
func (db *Database) listField(table, field string) (results []string, err error) {
	rows, err := db.Table(table).Select(fmt.Sprintf("DISTINCT %s", field)).Rows()
	defer rows.Close()
	if err != nil {
		return results, err
	}

	for rows.Next() {
		var t string
		rows.Scan(&t)
		results = append(results, t)
	}

	return
}

// listFieldWithCount is a wrapper utility method to list counts of values
// ordered by descending count from the Database
func (db *Database) listFieldWithCount(table, field string) (results []map[string]string, err error) {
	rows, err := db.Table(table).Select("?, COUNT(*)", field).
		Group(field).Order("count DESC").Rows()
	defer rows.Close()
	if err != nil {
		return results, err
	}

	for rows.Next() {
		var name string
		var count string
		r := make(map[string]string)
		rows.Scan(&name, &count)
		r["name"] = name
		r["count"] = count
		results = append(results, r)
	}

	return
}

// ListResourceTypes lists all Resource types from the Database
func (db *Database) ListResourceTypes() ([]string, error) {
	return db.listField("resources", "type")
}

//ListResourceTypesWithCount returns a list of Resource types with associated counts
//from the Database
func (db *Database) ListResourceTypesWithCount() (results []map[string]string, err error) {
	sql := "SELECT resources.type, COUNT(*)" +
		" FROM (SELECT DISTINCT ON(states.path) states.id, states.path, states.serial, states.tf_version, versions.version_id, versions.last_modified" +
		" FROM states" +
		" JOIN versions ON versions.id = states.version_id" +
		" ORDER BY states.path, versions.last_modified DESC) t" +
		" JOIN modules ON modules.state_id = t.id" +
		" JOIN resources ON resources.module_id = modules.id" +
		" GROUP BY resources.type" +
		" ORDER BY count DESC"

	rows, err := db.Raw(sql).Rows()
	defer rows.Close()
	if err != nil {
		return results, err
	}

	for rows.Next() {
		var name string
		var count string
		r := make(map[string]string)
		rows.Scan(&name, &count)
		r["name"] = name
		r["count"] = count
		results = append(results, r)
	}
	return
}

// ListResourceNames lists all Resource names from the Database
func (db *Database) ListResourceNames() ([]string, error) {
	return db.listField("resources", "name")
}

// ListTfVersions lists all Terraform versions from the Database
func (db *Database) ListTfVersions() ([]string, error) {
	return db.listField("states", "tf_version")
}

// ListAttributeKeys lists all Resource Attribute keys for a given Resource type
// from the Database
func (db *Database) ListAttributeKeys(resourceType string) (results []string, err error) {
	query := db.Table("attributes").
		Select("DISTINCT key").
		Joins("JOIN resources ON attributes.resource_id = resources.id")

	if resourceType != "" {
		query = query.Where("resources.type = ?", resourceType)
	}

	rows, err := query.Rows()
	defer rows.Close()
	if err != nil {
		return results, err
	}

	for rows.Next() {
		var t string
		rows.Scan(&t)
		results = append(results, t)
	}

	return
}

// DefaultVersion returns the detault VersionID for a given State path
func (db *Database) DefaultVersion(path string) (version string, err error) {
	sqlQuery := "SELECT versions.version_id FROM" +
		" (SELECT states.path, max(states.serial) as mx FROM states GROUP BY states.path) t" +
		" JOIN states ON t.path = states.path AND t.mx = states.serial" +
		" JOIN versions on states.version_id=versions.id" +
		" WHERE states.path = ?"

	row := db.Raw(sqlQuery, path).Row()
	row.Scan(&version)
	return
}
