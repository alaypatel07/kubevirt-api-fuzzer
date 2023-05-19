/*
This is largely inspired from https://github.com/kubernetes/apimachinery/blob/v0.26.3/pkg/api/apitesting/roundtrip/compatibility.go

and changed according to requirements of KubeVirt API
*/

package apitesting

import (
	"bytes"
	gojson "encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

// CompatibilityTestOptions holds configuration for running a compatibility test using in-memory objects
// and serialized files on disk representing the current code and serialized data from previous versions.
//
// Example use: `NewCompatibilityTestOptions(scheme).Complete(t).Run(t)`
type CompatibilityTestOptions struct {
	// Scheme is used to create new objects for filling, decoding, and for constructing serializers.
	// Required.
	Scheme *runtime.Scheme

	// TestDataDir points to a directory containing compatibility test data.
	// Complete() populates this with "testdata" if unset.
	TestDataDir string

	// TestDataDirCurrentVersion points to a directory containing compatibility test data for the current version.
	// Complete() populates this with "<TestDataDir>/HEAD" if unset.
	// Within this directory, `<group>.<version>.<kind>.[json|yaml|pb]` files are required to exist, and are:
	// * verified to match serialized FilledObjects[GVK]
	// * verified to decode without error
	// * verified to round-trip byte-for-byte when re-encoded
	// * verified to be semantically equal when decoded into memory
	TestDataDirCurrentVersion string

	// TestDataDirsPreviousVersions is a list of directories containing compatibility test data for previous versions.
	// Complete() populates this with "<TestDataDir>/v*" directories if nil.
	// Within these directories, `<group>.<version>.<kind>.[json|yaml|pb]` files are optional. If present, they are:
	// * verified to decode without error
	// * verified to round-trip byte-for-byte when re-encoded (or to match a `<group>.<version>.<kind>.[json|yaml|pb].after_roundtrip.[json|yaml|pb]` file if it exists)
	// * verified to be semantically equal when decoded into memory
	TestDataDirsPreviousVersions []string

	// Kinds is a list of fully qualified kinds to test.
	// Complete() populates this with Scheme.AllKnownTypes() if unset.
	Kinds []schema.GroupVersionKind

	// FilledObjects is an optional set of pre-filled objects to use for verifying HEAD fixtures.
	// Complete() populates this with the result of CompatibilityTestObject(Kinds[*], Scheme, FillFuncs) for any missing kinds.
	// Objects must deterministically populate every field and be identical on every invocation.
	FilledObjects map[schema.GroupVersionKind]runtime.Object

	// FillFuncs is an optional map of custom functions to use to fill instances of particular types.
	FillFuncs map[reflect.Type]FillFunc

	JSON  runtime.Serializer
	YAML  runtime.Serializer
	Proto runtime.Serializer
}

// FillFunc is a function that populates all serializable fields in obj.
// s and i are string and integer values relevant to the object being populated
// (for example, the json key or protobuf tag containing the object)
// that can be used when filling the object to make the object content identifiable
type FillFunc func(s string, i int, obj interface{})

func NewCompatibilityTestOptions(scheme *runtime.Scheme) *CompatibilityTestOptions {
	return &CompatibilityTestOptions{Scheme: scheme}
}

// coreKinds includes kinds that typically only need to be tested in a single API group
var coreKinds = sets.NewString(
	"CreateOptions", "UpdateOptions", "PatchOptions", "DeleteOptions",
	"GetOptions", "ListOptions", "ExportOptions",
	"WatchEvent",
)

func (c *CompatibilityTestOptions) Complete(t *testing.T) *CompatibilityTestOptions {
	t.Helper()

	// Verify scheme
	if c.Scheme == nil {
		t.Fatal("scheme is required")
	}

	// Populate testdata dirs
	if c.TestDataDir == "" {
		c.TestDataDir = "testdata"
	}
	if c.TestDataDirCurrentVersion == "" {
		c.TestDataDirCurrentVersion = filepath.Join(c.TestDataDir, "HEAD")
	}
	if c.TestDataDirsPreviousVersions == nil {
		dirs, err := filepath.Glob(filepath.Join(c.TestDataDir, "release-*"))
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(dirs)
		//fmt.Println(dirs)
		c.TestDataDirsPreviousVersions = dirs
		//fmt.Println(dirs)
	}

	// Populate kinds
	if len(c.Kinds) == 0 {
		gvks := []schema.GroupVersionKind{}
		for gvk := range c.Scheme.AllKnownTypes() {
			if gvk.Version == "" || gvk.Version == runtime.APIVersionInternal {
				// only test external types
				continue
			}
			if strings.HasSuffix(gvk.Kind, "List") {
				// omit list types
				continue
			}
			if gvk.Group != kubevirtv1.GroupVersion.Group && coreKinds.Has(gvk.Kind) {
				// only test options types in the core API group
				continue
			}
			gvks = append(gvks, gvk)
		}
		c.Kinds = gvks
	}

	// Sort kinds to get deterministic test order
	sort.Slice(c.Kinds, func(i, j int) bool {
		if c.Kinds[i].Group != c.Kinds[j].Group {
			return c.Kinds[i].Group < c.Kinds[j].Group
		}
		if c.Kinds[i].Version != c.Kinds[j].Version {
			return c.Kinds[i].Version < c.Kinds[j].Version
		}
		if c.Kinds[i].Kind != c.Kinds[j].Kind {
			return c.Kinds[i].Kind < c.Kinds[j].Kind
		}
		return false
	})

	//fmt.Println(c.Kinds)

	// Fill any missing objects
	if c.FilledObjects == nil {
		c.FilledObjects = map[schema.GroupVersionKind]runtime.Object{}
	}
	fillFuncs := defaultFillFuncs()
	for k, v := range c.FillFuncs {
		fillFuncs[k] = v
	}
	for _, gvk := range c.Kinds {
		if _, ok := c.FilledObjects[gvk]; ok {
			continue
		}
		//fmt.Println(gvk)
		obj, err := CompatibilityTestObject(c.Scheme, gvk, fillFuncs)
		if err != nil {
			t.Fatal(err)
		}
		c.FilledObjects[gvk] = obj
	}

	if c.JSON == nil {
		c.JSON = json.NewSerializer(json.DefaultMetaFactory, c.Scheme, c.Scheme, true)
	}
	if c.YAML == nil {
		c.YAML = json.NewYAMLSerializer(json.DefaultMetaFactory, c.Scheme, c.Scheme)
	}
	if c.Proto == nil {
		c.Proto = protobuf.NewSerializer(c.Scheme, c.Scheme)
	}

	return c
}

func (c *CompatibilityTestOptions) Run(t *testing.T) {
	usedHEADFixtures := sets.NewString()

	var ranCurrentTests bool
	for _, gvk := range c.Kinds {
		t.Run(makeName(gvk), func(t *testing.T) {

			t.Run("HEAD", func(t *testing.T) {
				c.runCurrentVersionTest(t, gvk, usedHEADFixtures)
				ranCurrentTests = true
			})

			for _, previousVersionDir := range c.TestDataDirsPreviousVersions {
				t.Run(filepath.Base(previousVersionDir), func(t *testing.T) {
					c.runPreviousVersionTest(t, gvk, previousVersionDir, nil)
				})
			}

		})
	}

	// Check for unused HEAD fixtures
	t.Run("unused_fixtures", func(t *testing.T) {
		if !ranCurrentTests {
			return
		}
		files, err := os.ReadDir(c.TestDataDirCurrentVersion)
		if err != nil {
			t.Fatal(err)
		}
		allFixtures := sets.NewString()
		for _, file := range files {
			allFixtures.Insert(file.Name())
		}

		if unused := allFixtures.Difference(usedHEADFixtures); len(unused) > 0 {
			t.Fatalf("remove unused fixtures from %s:\n%s", c.TestDataDirCurrentVersion, strings.Join(unused.List(), "\n"))
		}
	})
}

func (c *CompatibilityTestOptions) runCurrentVersionTest(t *testing.T, gvk schema.GroupVersionKind, usedFiles sets.String) {
	expectedObject := c.FilledObjects[gvk]
	expectedJSON, expectedYAML := c.encode(t, expectedObject)

	actualJSON, actualYAML, err := read(c.TestDataDirCurrentVersion, gvk, "", usedFiles)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	needsUpdate := false
	if os.IsNotExist(err) {
		t.Errorf("current version compatibility files did not exist: %v", err)
		needsUpdate = true
	} else {
		if !bytes.Equal(expectedJSON, actualJSON) {
			t.Errorf("json differs")
			t.Log(cmp.Diff(string(actualJSON), string(expectedJSON)))
			needsUpdate = true
		}

		if !bytes.Equal(expectedYAML, actualYAML) {
			t.Errorf("yaml differs")
			t.Log(cmp.Diff(string(actualYAML), string(expectedYAML)))
			needsUpdate = true
		}

		//if !bytes.Equal(expectedProto, actualProto) {
		//	t.Errorf("proto differs")
		//	needsUpdate = true
		//	t.Log(cmp.Diff(dumpProto(t, actualProto[4:]), dumpProto(t, expectedProto[4:])))
		// t.Logf("json (for locating the offending field based on surrounding data): %s", string(expectedJSON))
		//}
	}

	if needsUpdate {
		const updateEnvVar = "UPDATE_COMPATIBILITY_FIXTURE_DATA"
		if os.Getenv(updateEnvVar) == "true" {
			writeFile(t, c.TestDataDirCurrentVersion, gvk, "", "json", expectedJSON)
			writeFile(t, c.TestDataDirCurrentVersion, gvk, "", "yaml", expectedYAML)
			//writeFile(t, c.TestDataDirCurrentVersion, gvk, "", "pb", expectedProto)
			t.Logf("wrote expected compatibility data... verify, commit, and rerun tests")
		} else {
			t.Logf("if the diff is expected because of a new type or a new field, re-run with %s=true to update the compatibility data", updateEnvVar)
		}
		return
	}

	emptyObj, err := c.Scheme.New(gvk)
	if err != nil {
		t.Fatal(err)
	}
	{
		// compact before decoding since embedded RawExtension fields retain indenting
		compacted := &bytes.Buffer{}
		if err := gojson.Compact(compacted, actualJSON); err != nil {
			t.Error(err)
		}

		jsonDecoded := emptyObj.DeepCopyObject()
		jsonDecoded, _, err = c.JSON.Decode(compacted.Bytes(), &gvk, jsonDecoded)
		if err != nil {
			t.Error(err)
		} else if !apiequality.Semantic.DeepEqual(expectedObject, jsonDecoded) {
			t.Errorf("expected and decoded json objects differed:\n%s", cmp.Diff(expectedObject, jsonDecoded))
		}
	}
	{
		yamlDecoded := emptyObj.DeepCopyObject()
		yamlDecoded, _, err = c.YAML.Decode(actualYAML, &gvk, yamlDecoded)
		if err != nil {
			t.Error(err)
		} else if !apiequality.Semantic.DeepEqual(expectedObject, yamlDecoded) {
			t.Errorf("expected and decoded yaml objects differed:\n%s", cmp.Diff(expectedObject, yamlDecoded))
		}
	}
}

func (c *CompatibilityTestOptions) encode(t *testing.T, obj runtime.Object) (json, yaml []byte) {
	jsonBytes := bytes.NewBuffer(nil)
	if err := c.JSON.Encode(obj, jsonBytes); err != nil {
		t.Fatalf("error encoding json: %v", err)
	}
	yamlBytes := bytes.NewBuffer(nil)
	if err := c.YAML.Encode(obj, yamlBytes); err != nil {
		t.Fatalf("error encoding yaml: %v", err)
	}
	//protoBytes := bytes.NewBuffer(nil)
	//if err := c.Proto.Encode(obj, protoBytes); err != nil {
	//	t.Fatalf("error encoding proto: %v", err)
	//}
	return jsonBytes.Bytes(), yamlBytes.Bytes()
}

func read(dir string, gvk schema.GroupVersionKind, suffix string, usedFiles sets.String) (json, yaml []byte, err error) {
	jsonFilename := makeName(gvk) + suffix + ".json"
	actualJSON, jsonErr := ioutil.ReadFile(filepath.Join(dir, jsonFilename))
	yamlFilename := makeName(gvk) + suffix + ".yaml"
	actualYAML, yamlErr := ioutil.ReadFile(filepath.Join(dir, yamlFilename))
	//protoFilename := makeName(gvk) + suffix + ".pb"
	//actualProto, protoErr := ioutil.ReadFile(filepath.Join(dir, protoFilename))
	if usedFiles != nil {
		usedFiles.Insert(jsonFilename)
		usedFiles.Insert(yamlFilename)
		//usedFiles.Insert(protoFilename)
	}
	if jsonErr != nil {
		return actualJSON, actualYAML, jsonErr
	}
	if yamlErr != nil {
		return actualJSON, actualYAML, yamlErr
	}
	//if protoErr != nil {
	//	return actualJSON, actualYAML, protoErr
	//}
	//fmt.Println(actualJSON, actualYAML)
	return actualJSON, actualYAML, nil
}

func writeFile(t *testing.T, dir string, gvk schema.GroupVersionKind, suffix, extension string, data []byte) {
	if err := os.MkdirAll(dir, os.FileMode(0755)); err != nil {
		t.Fatal("error making directory", err)
	}
	if err := ioutil.WriteFile(filepath.Join(dir, makeName(gvk)+suffix+"."+extension), data, os.FileMode(0644)); err != nil {
		t.Fatalf("error writing %s: %v", extension, err)
	}
}

func (c *CompatibilityTestOptions) runPreviousVersionTest(t *testing.T, gvk schema.GroupVersionKind, previousVersionDir string, usedFiles sets.String) {
	jsonBeforeRoundTrip, yamlBeforeRoundTrip, err := read(previousVersionDir, gvk, "", usedFiles)
	if os.IsNotExist(err) || (len(jsonBeforeRoundTrip) == 0 && len(yamlBeforeRoundTrip) == 0) {
		fmt.Println("skipping")
		t.SkipNow()
		return
	}
	if err != nil {
		t.Fatal(err)
	}

	emptyObj, err := c.Scheme.New(gvk)
	if err != nil {
		t.Fatal(err)
	}

	// compact before decoding since embedded RawExtension fields retain indenting
	compacted := &bytes.Buffer{}
	if err := gojson.Compact(compacted, jsonBeforeRoundTrip); err != nil {
		t.Fatal(err)
	}

	jsonDecoded := emptyObj.DeepCopyObject()
	jsonDecoded, _, err = c.JSON.Decode(compacted.Bytes(), &gvk, jsonDecoded)
	if err != nil {
		t.Fatal(err)
	}
	jsonBytes := bytes.NewBuffer(nil)
	if err := c.JSON.Encode(jsonDecoded, jsonBytes); err != nil {
		t.Fatalf("error encoding json: %v", err)
	}
	jsonAfterRoundTrip := jsonBytes.Bytes()

	yamlDecoded := emptyObj.DeepCopyObject()
	yamlDecoded, _, err = c.YAML.Decode(yamlBeforeRoundTrip, &gvk, yamlDecoded)
	if err != nil {
		t.Fatal(err)
	} else if !apiequality.Semantic.DeepEqual(jsonDecoded, yamlDecoded) {
		t.Errorf("decoded json and yaml objects differ:\n%s", cmp.Diff(jsonDecoded, yamlDecoded))
	}
	yamlBytes := bytes.NewBuffer(nil)
	if err := c.YAML.Encode(yamlDecoded, yamlBytes); err != nil {
		t.Fatalf("error encoding yaml: %v", err)
	}
	yamlAfterRoundTrip := yamlBytes.Bytes()

	expectedJSONAfterRoundTrip, expectedYAMLAfterRoundTrip, _ := read(previousVersionDir, gvk, ".after_roundtrip", usedFiles)
	if len(expectedJSONAfterRoundTrip) == 0 {
		expectedJSONAfterRoundTrip = jsonBeforeRoundTrip
	}
	if len(expectedYAMLAfterRoundTrip) == 0 {
		expectedYAMLAfterRoundTrip = yamlBeforeRoundTrip
	}

	jsonNeedsUpdate := false
	yamlNeedsUpdate := false

	if !bytes.Equal(expectedJSONAfterRoundTrip, jsonAfterRoundTrip) {
		t.Errorf("json differs")
		t.Log(cmp.Diff(string(expectedJSONAfterRoundTrip), string(jsonAfterRoundTrip)))
		jsonNeedsUpdate = true
	}

	if !bytes.Equal(expectedYAMLAfterRoundTrip, yamlAfterRoundTrip) {
		t.Errorf("yaml differs")
		t.Log(cmp.Diff(string(expectedYAMLAfterRoundTrip), string(yamlAfterRoundTrip)))
		yamlNeedsUpdate = true
	}

	if jsonNeedsUpdate || yamlNeedsUpdate {
		const updateEnvVar = "UPDATE_COMPATIBILITY_FIXTURE_DATA"
		if os.Getenv(updateEnvVar) == "true" {
			if jsonNeedsUpdate {
				writeFile(t, previousVersionDir, gvk, ".after_roundtrip", "json", jsonAfterRoundTrip)
			}
			if yamlNeedsUpdate {
				writeFile(t, previousVersionDir, gvk, ".after_roundtrip", "yaml", yamlAfterRoundTrip)
			}
			t.Logf("wrote expected compatibility data... verify, commit, and rerun tests")
		} else {
			t.Logf("if the diff is expected because of a new type or a new field, re-run with %s=true to update the compatibility data", updateEnvVar)
		}
		return
	}
}

func makeName(gvk schema.GroupVersionKind) string {
	g := gvk.Group
	if g == "" {
		g = "core"
	}
	return g + "." + gvk.Version + "." + gvk.Kind
}

//func dumpProto(t *testing.T, data []byte) string {
//	t.Helper()
//	protoc, err := exec.LookPath("protoc")
//	if err != nil {
//		t.Log(err)
//		return ""
//	}
//	cmd := exec.Command(protoc, "--decode_raw")
//	cmd.Stdin = bytes.NewBuffer(data)
//	d, err := cmd.CombinedOutput()
//	if err != nil {
//		t.Log(err)
//		return ""
//	}
//	return string(d)
//}

func defaultFillFuncs() map[reflect.Type]FillFunc {
	funcs := map[reflect.Type]FillFunc{}
	funcs[reflect.TypeOf(&runtime.RawExtension{})] = func(s string, i int, obj interface{}) {
		// generate a raw object in normalized form
		// TODO: test non-normalized round-tripping... YAMLToJSON normalizes and makes exact comparisons fail
		obj.(*runtime.RawExtension).Raw = []byte(`{"apiVersion":"example.com/v1","kind":"CustomType","spec":{"replicas":1},"status":{"available":1}}`)
	}
	funcs[reflect.TypeOf(&metav1.TypeMeta{})] = func(s string, i int, obj interface{}) {
		// APIVersion and Kind are not serialized in all formats (notably protobuf), so clear by default for cross-format checking.
		obj.(*metav1.TypeMeta).APIVersion = ""
		obj.(*metav1.TypeMeta).Kind = ""
	}
	funcs[reflect.TypeOf(&metav1.FieldsV1{})] = func(s string, i int, obj interface{}) {
		obj.(*metav1.FieldsV1).Raw = []byte(`{}`)
	}
	funcs[reflect.TypeOf(&metav1.Time{})] = func(s string, i int, obj interface{}) {
		// use the integer as an offset from the year
		obj.(*metav1.Time).Time = time.Date(2000+i, 1, 1, 1, 1, 1, 0, time.UTC)
	}
	funcs[reflect.TypeOf(&metav1.MicroTime{})] = func(s string, i int, obj interface{}) {
		// use the integer as an offset from the year, and as a microsecond
		obj.(*metav1.MicroTime).Time = time.Date(2000+i, 1, 1, 1, 1, 1, i*int(time.Microsecond), time.UTC)
	}
	funcs[reflect.TypeOf(&intstr.IntOrString{})] = func(s string, i int, obj interface{}) {
		// use the string as a string value
		obj.(*intstr.IntOrString).Type = intstr.String
		obj.(*intstr.IntOrString).StrVal = s + "Value"
	}
	return funcs
}

// CompatibilityTestObject returns a deterministically filled object for the specified GVK
func CompatibilityTestObject(scheme *runtime.Scheme, gvk schema.GroupVersionKind, fillFuncs map[reflect.Type]FillFunc) (runtime.Object, error) {
	// Construct the object
	obj, err := scheme.New(gvk)
	if err != nil {
		return nil, err
	}

	fill("", 0, reflect.TypeOf(obj), reflect.ValueOf(obj), fillFuncs, map[reflect.Type]bool{})

	// Set the kind and apiVersion
	if typeAcc, err := apimeta.TypeAccessor(obj); err != nil {
		return nil, err
	} else {
		typeAcc.SetKind(gvk.Kind)
		typeAcc.SetAPIVersion(gvk.GroupVersion().String())
	}

	return obj, nil
}

func fill(dataString string, dataInt int, t reflect.Type, v reflect.Value, fillFuncs map[reflect.Type]FillFunc, filledTypes map[reflect.Type]bool) {
	if filledTypes[t] {
		// we already filled this type, avoid recursing infinitely
		return
	}
	filledTypes[t] = true
	defer delete(filledTypes, t)

	// if nil, populate pointers with a zero-value instance of the underlying type
	if t.Kind() == reflect.Pointer && v.IsNil() {
		if v.CanSet() {
			v.Set(reflect.New(t.Elem()))
		} else if v.IsNil() {
			panic(fmt.Errorf("unsettable nil pointer of type %v in field %s", t, dataString))
		}
	}

	if f, ok := fillFuncs[t]; ok {
		// use the custom fill function for this type
		f(dataString, dataInt, v.Interface())
		return
	}

	switch t.Kind() {
	case reflect.Slice:
		// populate with a single-item slice
		v.Set(reflect.MakeSlice(t, 1, 1))
		// recurse to populate the item, preserving the data context
		fill(dataString, dataInt, t.Elem(), v.Index(0), fillFuncs, filledTypes)

	case reflect.Map:
		// construct the key, which must be a string type, possibly converted to a type alias of string
		key := reflect.ValueOf(dataString + "Key").Convert(t.Key())
		// construct a zero-value item
		item := reflect.New(t.Elem())
		// recurse to populate the item, preserving the data context
		fill(dataString, dataInt, t.Elem(), item.Elem(), fillFuncs, filledTypes)
		// store in the map
		v.Set(reflect.MakeMap(t))
		v.SetMapIndex(key, item.Elem())

	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)

			if !field.IsExported() {
				continue
			}

			// use the json field name, which must be stable
			dataString := strings.Split(field.Tag.Get("json"), ",")[0]
			if len(dataString) == 0 {
				// fall back to the struct field name if there is no json field name
				dataString = "<no json tag> " + field.Name
			}

			// use the protobuf tag, which must be stable
			dataInt := 0
			if protobufTagParts := strings.Split(field.Tag.Get("protobuf"), ","); len(protobufTagParts) > 1 {
				if tag, err := strconv.Atoi(protobufTagParts[1]); err != nil {
					panic(err)
				} else {
					dataInt = tag
				}
			}
			if dataInt == 0 {
				// fall back to the length of dataString as a backup
				dataInt = -len(dataString)
			}

			fieldType := field.Type
			fieldValue := v.Field(i)

			fill(dataString, dataInt, reflect.PointerTo(fieldType), fieldValue.Addr(), fillFuncs, filledTypes)
		}

	case reflect.Pointer:
		fill(dataString, dataInt, t.Elem(), v.Elem(), fillFuncs, filledTypes)

	case reflect.String:
		// use Convert to set into string alias types correctly
		v.Set(reflect.ValueOf(dataString + "Value").Convert(t))

	case reflect.Bool:
		// set to true to ensure we serialize omitempty fields
		v.Set(reflect.ValueOf(true).Convert(t))

	case reflect.Int, reflect.Uint, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// use Convert to set into int alias types and different int widths correctly
		v.Set(reflect.ValueOf(dataInt).Convert(t))
	case reflect.Float64, reflect.Float32:
		v.Set(reflect.ValueOf(dataInt).Convert(t))

	default:
		panic(fmt.Errorf("unhandled type %v in field %s", t, dataString))
	}
}
