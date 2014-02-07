package storage

import (
	"labix.org/v2/mgo"
	"github.com/contester/runlib/contester_proto"
	"github.com/contester/runlib/tools"
	"fmt"
	"os"
	"compress/zlib"
	"io"
	"labix.org/v2/mgo/bson"
	"strings"
)

type mongodbStorage struct {
	URL string
	*mgo.Session
	*mgo.Database
	*mgo.GridFS
}

func NewMongoDB(url string) (Backend, error) {
	var result mongodbStorage
	var err error
	result.URL = url
	result.Session, err = mgo.Dial(result.URL)
	if err != nil {
		return nil, err
	}
	result.Database = result.Session.DB("")
	result.GridFS = result.Database.GridFS("fs")
	return &result, nil
}

func (s *mongodbStorage) String() string {
	return s.URL
}

func (s *mongodbStorage) Close() {
	s.Session.Close()
}

type fileMetadata struct {
	Checksum string `bson:"checksum,omitempty"`
	ModuleType string `bson:"moduleType,omitempty"`
	CompressionType string `bson:"compressionType,omitempty"`
	OriginalSize uint64 `bson:"originalSize"`
}

func (s *mongodbStorage) Copy(localName, remoteName string, toRemote bool, checksum, moduleType string) (stat *contester_proto.FileStat, err error) {
	ec := tools.NewContext("mongodb.Copy")

	if toRemote {
		stat, err = tools.StatFile(localName, true)
		if err != nil {
			err = ec.NewError(err, "local.CalculateChecksum")
		}
		// If file doesn't exist then stat == nil.
		if err != nil || stat == nil {
			return
		}

		if checksum != "" && *stat.Checksum != checksum {
			return nil, ec.NewError(fmt.Errorf("Checksum mismatch, local %s != %s", stat.Checksum, checksum))
		}

		checksum = *stat.Checksum
	}

	var local *os.File
	if toRemote {
		local, err = os.Open(localName)
	} else {
		local, err = os.Create(localName)
	}

	if err != nil {
		return nil, ec.NewError(err, "local.Open")
	}
	defer local.Close()

	var remote *mgo.GridFile
	if toRemote {
		// Remove all files with the same remoteName.
		if err = s.GridFS.Remove(remoteName); err != nil {
			return nil, ec.NewError(err, "remote.Remove")
		}
		remote, err = s.GridFS.Create(remoteName)
	} else {
		remote, err = s.GridFS.Open(remoteName)
	}
	if err != nil {
		return nil, ec.NewError(err, "remote.Open")
	}
	defer remote.Close()

	var source io.ReadCloser
	if toRemote {
		source = local
	} else {
		source = remote
		var meta fileMetadata
		if err = remote.GetMeta(&meta); err != nil {
			return nil, ec.NewError(err, "remote.GetMeta")
		}
		if meta.CompressionType == "ZLIB" {
			source, err = zlib.NewReader(source)
			if err != nil {
				return nil, ec.NewError(err, "zlib.NewReader")
			}
		}
	}

	var destination io.WriteCloser
	if toRemote {
		destination = zlib.NewWriter(remote)
	} else {
		destination = local
	}

	size, err := io.Copy(destination, source)
	if err != nil {
		return nil, ec.NewError(err, "io.Copy")
	}

	if toRemote {
		var meta fileMetadata
		meta.OriginalSize = uint64(size)
		meta.CompressionType = "ZLIB"
		meta.Checksum = *stat.Checksum
		meta.ModuleType = moduleType

		remote.SetMeta(meta)
	}

	if err = destination.Close(); err != nil {
		return nil, ec.NewError(err, "destination.Close")
	}

	if err = source.Close(); err != nil {
		return nil, ec.NewError(err, "source.Close")
	}

	if !toRemote {
		stat, err = tools.StatFile(localName, true)
		if err != nil {
			return nil, ec.NewError(err, "StatFile")
		}
	}

	return stat, nil
}

func (s *mongodbStorage) GetNextRevision(id string) (int, error) {
	query := s.Database.C("manifest").Find(bson.M{"id": id}).Sort("-revision")
var manifest ProblemManifest
if err := query.One(&manifest); err != nil {
return 1, nil
}
return manifest.Revision + 1, nil
}

func (s *mongodbStorage) SetManifest(manifest *ProblemManifest) error {
	return s.Database.C("manifest").Insert(manifest)
}

func (s *mongodbStorage) getAllProblemIds() []string {
	var ids []string
	s.Database.C("manifest").Find(nil).Distinct("id", &ids)
	return ids
}

func (s *mongodbStorage) doCleanup(id string, latest int) error {
	iter := s.Database.C("manifest").Find(bson.M{"id": id}).Sort("-revision").Iter()
	defer iter.Close()
	var manifest ProblemManifest

	for iter.Next(&manifest) {
		if latest--; latest >= 0 {
			continue
		}
		s.Database.C("manifest").RemoveId(manifest.Key)
	}
	return nil
}

func (s *mongodbStorage) getAllGridPrefixes() []string {
	var ids []string
	iter := s.Database.C("manifest").Find(nil).Iter()
	defer iter.Close()
	var m ProblemManifest
	for iter.Next(&m) {
		ids = append(ids, m.GetGridPrefix())
	}
	return ids
}

func hasAnyPrefix(s string, p []string) bool {
	for _, v := range p {
		if strings.HasPrefix(s, v) {
			return true
		}
	}
	return false
}

func (s *mongodbStorage) Cleanup(latest int) error {
	pids := s.getAllProblemIds()
	for _, v := range pids {
		s.doCleanup(v, latest)
	}

	pids = s.getAllGridPrefixes()
	iter := s.GridFS.Find(nil).Sort("filename").Iter()
	var f *mgo.GridFile
	for s.GridFS.OpenNext(iter, &f) {
		if !strings.HasPrefix(f.Name(), "problem/") {
			continue
		}
		if !hasAnyPrefix(f.Name(), pids) {
			fmt.Printf("Remove: %s\n", f.Name())
			s.GridFS.RemoveId(f.Id())
		}
	}
	return nil
}
