/*
 * Minio Cloud Storage, (C) 2016, 2017 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/teamwork/minio/pkg/disk"
	"github.com/teamwork/minio/pkg/lock"
)

// fsObjects - Implements fs object layer.
type fsObjects struct {
	// Path to be exported over S3 API.
	fsPath string

	// Unique value to be used for all
	// temporary transactions.
	fsUUID string

	minFreeSpace  int64
	minFreeInodes int64

	// FS rw pool.
	rwPool *fsIOPool

	// ListObjects pool management.
	listPool *treeWalkPool

	// To manage the appendRoutine go0routines
	bgAppend *backgroundAppend
}

// Initializes meta volume on all the fs path.
func initMetaVolumeFS(fsPath, fsUUID string) error {
	// This happens for the first time, but keep this here since this
	// is the only place where it can be made less expensive
	// optimizing all other calls. Create minio meta volume,
	// if it doesn't exist yet.
	metaBucketPath := pathJoin(fsPath, minioMetaBucket)
	if err := mkdirAll(metaBucketPath, 0777); err != nil {
		return err
	}

	metaTmpPath := pathJoin(fsPath, minioMetaTmpBucket, fsUUID)
	if err := mkdirAll(metaTmpPath, 0777); err != nil {
		return err
	}

	metaMultipartPath := pathJoin(fsPath, minioMetaMultipartBucket)
	if err := mkdirAll(metaMultipartPath, 0777); err != nil {
		return err
	}

	// Return success here.
	return nil

}

// newFSObjectLayer - initialize new fs object layer.
func newFSObjectLayer(fsPath string) (ObjectLayer, error) {
	if fsPath == "" {
		return nil, errInvalidArgument
	}

	var err error
	// Disallow relative paths, figure out absolute paths.
	fsPath, err = filepath.Abs(fsPath)
	if err != nil {
		return nil, err
	}

	fi, err := os.Stat(preparePath(fsPath))
	if err == nil {
		if !fi.IsDir() {
			return nil, syscall.ENOTDIR
		}
	}
	if os.IsNotExist(err) {
		// Disk not found create it.
		err = mkdirAll(fsPath, 0777)
		if err != nil {
			return nil, err
		}
	}

	// Assign a new UUID for FS minio mode. Each server instance
	// gets its own UUID for temporary file transaction.
	fsUUID := mustGetUUID()

	// Initialize meta volume, if volume already exists ignores it.
	if err = initMetaVolumeFS(fsPath, fsUUID); err != nil {
		return nil, fmt.Errorf("Unable to initialize '.minio.sys' meta volume, %s", err)
	}

	// Load `format.json`.
	format, err := loadFormatFS(fsPath)
	if err != nil && err != errUnformattedDisk {
		return nil, fmt.Errorf("Unable to load 'format.json', %s", err)
	}

	// If the `format.json` doesn't exist create one.
	if err == errUnformattedDisk {
		fsFormatPath := pathJoin(fsPath, minioMetaBucket, fsFormatJSONFile)
		// Initialize format.json, if already exists overwrite it.
		if serr := saveFormatFS(fsFormatPath, newFSFormatV1()); serr != nil {
			return nil, fmt.Errorf("Unable to initialize 'format.json', %s", serr)
		}
	}

	// Validate if we have the same format.
	if err == nil && format.Format != "fs" {
		return nil, fmt.Errorf("Unable to recognize backend format, Disk is not in FS format. %s", format.Format)
	}

	// Initialize fs objects.
	fs := &fsObjects{
		fsPath:        fsPath,
		fsUUID:        fsUUID,
		minFreeSpace:  fsMinFreeSpace,
		minFreeInodes: fsMinFreeInodes,
		rwPool: &fsIOPool{
			readersMap: make(map[string]*lock.RLockedFile),
		},
		listPool: newTreeWalkPool(globalLookupTimeout),
		bgAppend: &backgroundAppend{
			infoMap: make(map[string]bgAppendPartsInfo),
		},
	}

	// Initialize and load bucket policies.
	err = initBucketPolicies(fs)
	if err != nil {
		return nil, fmt.Errorf("Unable to load all bucket policies. %s", err)
	}

	// Initialize a new event notifier.
	err = initEventNotifier(fs)
	if err != nil {
		return nil, fmt.Errorf("Unable to initialize event notification. %s", err)
	}

	// Return successfully initialized object layer.
	return fs, nil
}

// checkDiskFree verifies if disk path has sufficient minimum free disk space and files.
func (fs fsObjects) checkDiskFree() (err error) {
	// We don't validate disk space or inode utilization on windows.
	// Each windows calls to 'GetVolumeInformationW' takes around 3-5seconds.
	if runtime.GOOS == globalWindowsOSName {
		return nil
	}

	var di disk.Info
	di, err = getDiskInfo(preparePath(fs.fsPath))
	if err != nil {
		return err
	}

	// Remove 5% from free space for cumulative disk space used for journalling, inodes etc.
	availableDiskSpace := float64(di.Free) * 0.95
	if int64(availableDiskSpace) <= fs.minFreeSpace {
		return errDiskFull
	}

	// Some filesystems do not implement a way to provide total inodes available, instead inodes
	// are allocated based on available disk space. For example CephFS, StoreNext CVFS, AzureFile driver.
	// Allow for the available disk to be separately validate and we will validate inodes only if
	// total inodes are provided by the underlying filesystem.
	if di.Files != 0 && di.FSType != "NFS" {
		availableFiles := int64(di.Ffree)
		if availableFiles <= fs.minFreeInodes {
			return errDiskFull
		}
	}

	// Success.
	return nil
}

// Should be called when process shuts down.
func (fs fsObjects) Shutdown() error {
	// Cleanup and delete tmp uuid.
	return fsRemoveAll(pathJoin(fs.fsPath, minioMetaTmpBucket, fs.fsUUID))
}

// StorageInfo - returns underlying storage statistics.
func (fs fsObjects) StorageInfo() StorageInfo {
	info, err := getDiskInfo(preparePath(fs.fsPath))
	errorIf(err, "Unable to get disk info %#v", fs.fsPath)
	storageInfo := StorageInfo{
		Total: info.Total,
		Free:  info.Free,
	}
	storageInfo.Backend.Type = FS
	return storageInfo
}

/// Bucket operations

// getBucketDir - will convert incoming bucket names to
// corresponding valid bucket names on the backend in a platform
// compatible way for all operating systems.
func (fs fsObjects) getBucketDir(bucket string) (string, error) {
	// Verify if bucket is valid.
	if !IsValidBucketName(bucket) {
		return "", traceError(BucketNameInvalid{Bucket: bucket})
	}

	bucketDir := pathJoin(fs.fsPath, bucket)
	return bucketDir, nil
}

func (fs fsObjects) statBucketDir(bucket string) (os.FileInfo, error) {
	bucketDir, err := fs.getBucketDir(bucket)
	if err != nil {
		return nil, err
	}
	st, err := fsStatDir(bucketDir)
	if err != nil {
		return nil, traceError(err)
	}
	return st, nil
}

// MakeBucket - create a new bucket, returns if it
// already exists.
func (fs fsObjects) MakeBucket(bucket string) error {
	bucketDir, err := fs.getBucketDir(bucket)
	if err != nil {
		return toObjectErr(err, bucket)
	}

	if err = fsMkdir(bucketDir); err != nil {
		return toObjectErr(traceError(err), bucket)
	}

	return nil
}

// GetBucketInfo - fetch bucket metadata info.
func (fs fsObjects) GetBucketInfo(bucket string) (BucketInfo, error) {
	st, err := fs.statBucketDir(bucket)
	if err != nil {
		return BucketInfo{}, toObjectErr(err, bucket)
	}

	// As os.Stat() doesn't carry other than ModTime(), use ModTime() as CreatedTime.
	createdTime := st.ModTime()
	return BucketInfo{
		Name:    bucket,
		Created: createdTime,
	}, nil
}

// ListBuckets - list all s3 compatible buckets (directories) at fsPath.
func (fs fsObjects) ListBuckets() ([]BucketInfo, error) {
	if err := checkPathLength(fs.fsPath); err != nil {
		return nil, err
	}
	var bucketInfos []BucketInfo
	entries, err := readDir(preparePath(fs.fsPath))
	if err != nil {
		return nil, toObjectErr(traceError(errDiskNotFound))
	}

	var invalidBucketNames []string
	for _, entry := range entries {
		if entry == minioMetaBucket+"/" || !strings.HasSuffix(entry, slashSeparator) {
			continue
		}

		var fi os.FileInfo
		fi, err = fsStatDir(pathJoin(fs.fsPath, entry))
		if err != nil {
			// If the directory does not exist, skip the entry.
			if err == errVolumeNotFound {
				continue
			} else if err == errVolumeAccessDenied {
				// Skip the entry if its a file.
				continue
			}
			return nil, err
		}

		if !IsValidBucketName(fi.Name()) {
			invalidBucketNames = append(invalidBucketNames, fi.Name())
			continue
		}

		bucketInfos = append(bucketInfos, BucketInfo{
			Name: fi.Name(),
			// As os.Stat() doesn't carry other than ModTime(), use ModTime() as CreatedTime.
			Created: fi.ModTime(),
		})
	}

	// Print a user friendly message if we indeed skipped certain directories which are
	// incompatible with S3's bucket name restrictions.
	if len(invalidBucketNames) > 0 {
		errorIf(errors.New("One or more invalid bucket names found"), "Skipping %s", invalidBucketNames)
	}

	// Sort bucket infos by bucket name.
	sort.Sort(byBucketName(bucketInfos))

	// Succes.
	return bucketInfos, nil
}

// DeleteBucket - delete a bucket and all the metadata associated
// with the bucket including pending multipart, object metadata.
func (fs fsObjects) DeleteBucket(bucket string) error {
	bucketDir, err := fs.getBucketDir(bucket)
	if err != nil {
		return toObjectErr(err, bucket)
	}

	// Attempt to delete regular bucket.
	if err = fsRemoveDir(bucketDir); err != nil {
		return toObjectErr(err, bucket)
	}

	// Cleanup all the previously incomplete multiparts.
	minioMetaMultipartBucketDir := pathJoin(fs.fsPath, minioMetaMultipartBucket, bucket)
	if err = fsRemoveAll(minioMetaMultipartBucketDir); err != nil {
		return toObjectErr(err, bucket)
	}

	// Cleanup all the bucket metadata.
	minioMetadataBucketDir := pathJoin(fs.fsPath, minioMetaBucket, bucketMetaPrefix, bucket)
	if err = fsRemoveAll(minioMetadataBucketDir); err != nil {
		return toObjectErr(err, bucket)
	}

	return nil
}

/// Object Operations

// CopyObject - copy object source object to destination object.
// if source object and destination object are same we only
// update metadata.
func (fs fsObjects) CopyObject(srcBucket, srcObject, dstBucket, dstObject string, metadata map[string]string) (ObjectInfo, error) {
	if _, err := fs.statBucketDir(srcBucket); err != nil {
		return ObjectInfo{}, toObjectErr(err, srcBucket)
	}

	// Stat the file to get file size.
	fi, err := fsStatFile(pathJoin(fs.fsPath, srcBucket, srcObject))
	if err != nil {
		return ObjectInfo{}, toObjectErr(traceError(err), srcBucket, srcObject)
	}

	// Check if this request is only metadata update.
	cpMetadataOnly := strings.EqualFold(pathJoin(srcBucket, srcObject), pathJoin(dstBucket, dstObject))
	if cpMetadataOnly {
		fsMetaPath := pathJoin(fs.fsPath, minioMetaBucket, bucketMetaPrefix, srcBucket, srcObject, fsMetaJSONFile)
		var wlk *lock.LockedFile
		wlk, err = fs.rwPool.Write(fsMetaPath)
		if err != nil {
			return ObjectInfo{}, toObjectErr(err, srcBucket, srcObject)
		}
		// This close will allow for locks to be synchronized on `fs.json`.
		defer wlk.Close()

		// Save objects' metadata in `fs.json`.
		fsMeta := newFSMetaV1()
		fsMeta.Meta = metadata
		if _, err = fsMeta.WriteTo(wlk); err != nil {
			return ObjectInfo{}, toObjectErr(err, srcBucket, srcObject)
		}

		// Return the new object info.
		return fsMeta.ToObjectInfo(srcBucket, srcObject, fi), nil
	}

	// Length of the file to read.
	length := fi.Size()

	// Initialize pipe.
	pipeReader, pipeWriter := io.Pipe()

	go func() {
		startOffset := int64(0) // Read the whole file.
		if gerr := fs.GetObject(srcBucket, srcObject, startOffset, length, pipeWriter); gerr != nil {
			errorIf(gerr, "Unable to read %s/%s.", srcBucket, srcObject)
			pipeWriter.CloseWithError(gerr)
			return
		}
		pipeWriter.Close() // Close writer explicitly signalling we wrote all data.
	}()

	objInfo, err := fs.PutObject(dstBucket, dstObject, length, pipeReader, metadata, "")
	if err != nil {
		return ObjectInfo{}, toObjectErr(err, dstBucket, dstObject)
	}

	// Explicitly close the reader.
	pipeReader.Close()

	return objInfo, nil
}

// GetObject - reads an object from the disk.
// Supports additional parameters like offset and length
// which are synonymous with HTTP Range requests.
//
// startOffset indicates the starting read location of the object.
// length indicates the total length of the object.
func (fs fsObjects) GetObject(bucket, object string, offset int64, length int64, writer io.Writer) (err error) {
	if err = checkGetObjArgs(bucket, object); err != nil {
		return err
	}

	if _, err = fs.statBucketDir(bucket); err != nil {
		return toObjectErr(err, bucket)
	}

	// Offset cannot be negative.
	if offset < 0 {
		return toObjectErr(traceError(errUnexpected), bucket, object)
	}

	// Writer cannot be nil.
	if writer == nil {
		return toObjectErr(traceError(errUnexpected), bucket, object)
	}

	if bucket != minioMetaBucket {
		fsMetaPath := pathJoin(fs.fsPath, minioMetaBucket, bucketMetaPrefix, bucket, object, fsMetaJSONFile)
		_, err = fs.rwPool.Open(fsMetaPath)
		if err != nil && err != errFileNotFound {
			return toObjectErr(traceError(err), bucket, object)
		}
		defer fs.rwPool.Close(fsMetaPath)
	}

	// Read the object, doesn't exist returns an s3 compatible error.
	fsObjPath := pathJoin(fs.fsPath, bucket, object)
	reader, size, err := fsOpenFile(fsObjPath, offset)
	if err != nil {
		return toObjectErr(traceError(err), bucket, object)
	}
	defer reader.Close()

	bufSize := int64(readSizeV1)
	if length > 0 && bufSize > length {
		bufSize = length
	}

	// For negative length we read everything.
	if length < 0 {
		length = size - offset
	}

	// Reply back invalid range if the input offset and length fall out of range.
	if offset > size || offset+length > size {
		return traceError(InvalidRange{offset, length, size})
	}

	// Allocate a staging buffer.
	buf := make([]byte, int(bufSize))

	_, err = io.CopyBuffer(writer, io.LimitReader(reader, length), buf)

	return toObjectErr(traceError(err), bucket, object)
}

// getObjectInfo - wrapper for reading object metadata and constructs ObjectInfo.
func (fs fsObjects) getObjectInfo(bucket, object string) (ObjectInfo, error) {
	fsMeta := fsMetaV1{}
	fsMetaPath := pathJoin(fs.fsPath, minioMetaBucket, bucketMetaPrefix, bucket, object, fsMetaJSONFile)

	// Read `fs.json` to perhaps contend with
	// parallel Put() operations.
	rlk, err := fs.rwPool.Open(fsMetaPath)
	if err == nil {
		// Read from fs metadata only if it exists.
		defer fs.rwPool.Close(fsMetaPath)
		if _, rerr := fsMeta.ReadFrom(io.NewSectionReader(rlk, 0, rlk.Size())); rerr != nil {
			return ObjectInfo{}, toObjectErr(rerr, bucket, object)
		}
	}

	// Ignore if `fs.json` is not available, this is true for pre-existing data.
	if err != nil && err != errFileNotFound {
		return ObjectInfo{}, toObjectErr(traceError(err), bucket, object)
	}

	// Stat the file to get file size.
	fi, err := fsStatFile(pathJoin(fs.fsPath, bucket, object))
	if err != nil {
		return ObjectInfo{}, toObjectErr(traceError(err), bucket, object)
	}

	return fsMeta.ToObjectInfo(bucket, object, fi), nil
}

// GetObjectInfo - reads object metadata and replies back ObjectInfo.
func (fs fsObjects) GetObjectInfo(bucket, object string) (ObjectInfo, error) {
	if err := checkGetObjArgs(bucket, object); err != nil {
		return ObjectInfo{}, err
	}

	if _, err := fs.statBucketDir(bucket); err != nil {
		return ObjectInfo{}, toObjectErr(err, bucket)
	}

	return fs.getObjectInfo(bucket, object)
}

// PutObject - creates an object upon reading from the input stream
// until EOF, writes data directly to configured filesystem path.
// Additionally writes `fs.json` which carries the necessary metadata
// for future object operations.
func (fs fsObjects) PutObject(bucket string, object string, size int64, data io.Reader, metadata map[string]string, sha256sum string) (objInfo ObjectInfo, err error) {
	// This is a special case with size as '0' and object ends with
	// a slash separator, we treat it like a valid operation and
	// return success.
	if isObjectDir(object, size) {
		return dirObjectInfo(bucket, object, size, metadata), nil
	}
	if err = checkPutObjectArgs(bucket, object, fs); err != nil {
		return ObjectInfo{}, err
	}

	if _, err = fs.statBucketDir(bucket); err != nil {
		return ObjectInfo{}, toObjectErr(err, bucket)
	}

	// No metadata is set, allocate a new one.
	if metadata == nil {
		metadata = make(map[string]string)
	}

	fsMeta := newFSMetaV1()
	fsMeta.Meta = metadata

	var wlk *lock.LockedFile
	if bucket != minioMetaBucket {
		fsMetaPath := pathJoin(fs.fsPath, minioMetaBucket, bucketMetaPrefix, bucket, object, fsMetaJSONFile)
		wlk, err = fs.rwPool.Create(fsMetaPath)
		if err != nil {
			return ObjectInfo{}, toObjectErr(err, bucket, object)
		}
		// This close will allow for locks to be synchronized on `fs.json`.
		defer wlk.Close()
	}

	// Uploaded object will first be written to the temporary location which will eventually
	// be renamed to the actual location. It is first written to the temporary location
	// so that cleaning it up will be easy if the server goes down.
	tempObj := mustGetUUID()

	// Initialize md5 writer.
	md5Writer := md5.New()

	hashWriters := []io.Writer{md5Writer}

	var sha256Writer hash.Hash
	if sha256sum != "" {
		sha256Writer = sha256.New()
		hashWriters = append(hashWriters, sha256Writer)
	}
	multiWriter := io.MultiWriter(hashWriters...)

	// Limit the reader to its provided size if specified.
	var limitDataReader io.Reader
	if size > 0 {
		// This is done so that we can avoid erroneous clients sending more data than the set content size.
		limitDataReader = io.LimitReader(data, size)
	} else {
		// else we read till EOF.
		limitDataReader = data
	}

	// Allocate a buffer to Read() from request body
	bufSize := int64(readSizeV1)
	if size > 0 && bufSize > size {
		bufSize = size
	}
	buf := make([]byte, int(bufSize))
	teeReader := io.TeeReader(limitDataReader, multiWriter)
	fsTmpObjPath := pathJoin(fs.fsPath, minioMetaTmpBucket, fs.fsUUID, tempObj)
	bytesWritten, err := fsCreateFile(fsTmpObjPath, teeReader, buf, size)
	if err != nil {
		fsRemoveFile(fsTmpObjPath)
		errorIf(err, "Failed to create object %s/%s", bucket, object)
		return ObjectInfo{}, toObjectErr(err, bucket, object)
	}

	// Should return IncompleteBody{} error when reader has fewer
	// bytes than specified in request header.
	if bytesWritten < size {
		fsRemoveFile(fsTmpObjPath)
		return ObjectInfo{}, traceError(IncompleteBody{})
	}

	// Delete the temporary object in the case of a
	// failure. If PutObject succeeds, then there would be
	// nothing to delete.
	defer fsRemoveFile(fsTmpObjPath)

	newMD5Hex := hex.EncodeToString(md5Writer.Sum(nil))
	// Update the md5sum if not set with the newly calculated one.
	if len(metadata["md5Sum"]) == 0 {
		metadata["md5Sum"] = newMD5Hex
	}

	// md5Hex representation.
	md5Hex := metadata["md5Sum"]
	if md5Hex != "" {
		if newMD5Hex != md5Hex {
			// Returns md5 mismatch.
			return ObjectInfo{}, traceError(BadDigest{md5Hex, newMD5Hex})
		}
	}

	if sha256sum != "" {
		newSHA256sum := hex.EncodeToString(sha256Writer.Sum(nil))
		if newSHA256sum != sha256sum {
			return ObjectInfo{}, traceError(SHA256Mismatch{})
		}
	}

	// Entire object was written to the temp location, now it's safe to rename it to the actual location.
	fsNSObjPath := pathJoin(fs.fsPath, bucket, object)
	if err = fsRenameFile(fsTmpObjPath, fsNSObjPath); err != nil {
		return ObjectInfo{}, toObjectErr(err, bucket, object)
	}

	if bucket != minioMetaBucket {
		// Write FS metadata after a successful namespace operation.
		if _, err = fsMeta.WriteTo(wlk); err != nil {
			return ObjectInfo{}, toObjectErr(err, bucket, object)
		}
	}

	// Stat the file to fetch timestamp, size.
	fi, err := fsStatFile(pathJoin(fs.fsPath, bucket, object))
	if err != nil {
		return ObjectInfo{}, toObjectErr(traceError(err), bucket, object)
	}

	// Success.
	return fsMeta.ToObjectInfo(bucket, object, fi), nil
}

// DeleteObject - deletes an object from a bucket, this operation is destructive
// and there are no rollbacks supported.
func (fs fsObjects) DeleteObject(bucket, object string) error {
	if err := checkDelObjArgs(bucket, object); err != nil {
		return err
	}

	if _, err := fs.statBucketDir(bucket); err != nil {
		return toObjectErr(err, bucket)
	}

	minioMetaBucketDir := pathJoin(fs.fsPath, minioMetaBucket)
	fsMetaPath := pathJoin(minioMetaBucketDir, bucketMetaPrefix, bucket, object, fsMetaJSONFile)
	if bucket != minioMetaBucket {
		rwlk, lerr := fs.rwPool.Write(fsMetaPath)
		if lerr == nil {
			// This close will allow for fs locks to be synchronized on `fs.json`.
			defer rwlk.Close()
		}
		if lerr != nil && lerr != errFileNotFound {
			return toObjectErr(lerr, bucket, object)
		}
	}

	// Delete the object.
	if err := fsDeleteFile(pathJoin(fs.fsPath, bucket), pathJoin(fs.fsPath, bucket, object)); err != nil {
		return toObjectErr(traceError(err), bucket, object)
	}

	if bucket != minioMetaBucket {
		// Delete the metadata object.
		err := fsDeleteFile(minioMetaBucketDir, fsMetaPath)
		if err != nil && err != errFileNotFound {
			return toObjectErr(traceError(err), bucket, object)
		}
	}
	return nil
}

// list of all errors that can be ignored in tree walk operation in FS
var fsTreeWalkIgnoredErrs = append(baseIgnoredErrs, []error{
	errFileNotFound,
	errVolumeNotFound,
}...)

// Returns function "listDir" of the type listDirFunc.
// isLeaf - is used by listDir function to check if an entry
// is a leaf or non-leaf entry.
func (fs fsObjects) listDirFactory(isLeaf isLeafFunc) listDirFunc {
	// listDir - lists all the entries at a given prefix and given entry in the prefix.
	listDir := func(bucket, prefixDir, prefixEntry string) (entries []string, delayIsLeaf bool, err error) {
		entries, err = readDir(pathJoin(fs.fsPath, bucket, prefixDir))
		if err == nil {
			// Listing needs to be sorted.
			sort.Strings(entries)

			// Filter entries that have the prefix prefixEntry.
			entries = filterMatchingPrefix(entries, prefixEntry)

			// Can isLeaf() check be delayed till when it has to be sent down the
			// treeWalkResult channel?
			delayIsLeaf = delayIsLeafCheck(entries)
			if delayIsLeaf {
				return entries, delayIsLeaf, nil
			}

			// isLeaf() check has to happen here so that trailing "/" for objects can be removed.
			for i, entry := range entries {
				if isLeaf(bucket, pathJoin(prefixDir, entry)) {
					entries[i] = strings.TrimSuffix(entry, slashSeparator)
				}
			}

			// Sort again after removing trailing "/" for objects as the previous sort
			// does not hold good anymore.
			sort.Strings(entries)

			// Succes.
			return entries, delayIsLeaf, nil
		} // Return error at the end.

		// Error.
		return nil, false, err
	}

	// Return list factory instance.
	return listDir
}

// ListObjects - list all objects at prefix upto maxKeys., optionally delimited by '/'. Maintains the list pool
// state for future re-entrant list requests.
func (fs fsObjects) ListObjects(bucket, prefix, marker, delimiter string, maxKeys int) (ListObjectsInfo, error) {
	if err := checkListObjsArgs(bucket, prefix, marker, delimiter, fs); err != nil {
		return ListObjectsInfo{}, err
	}

	if _, err := fs.statBucketDir(bucket); err != nil {
		return ListObjectsInfo{}, err
	}

	// With max keys of zero we have reached eof, return right here.
	if maxKeys == 0 {
		return ListObjectsInfo{}, nil
	}

	// For delimiter and prefix as '/' we do not list anything at all
	// since according to s3 spec we stop at the 'delimiter'
	// along // with the prefix. On a flat namespace with 'prefix'
	// as '/' we don't have any entries, since all the keys are
	// of form 'keyName/...'
	if delimiter == slashSeparator && prefix == slashSeparator {
		return ListObjectsInfo{}, nil
	}

	// Over flowing count - reset to maxObjectList.
	if maxKeys < 0 || maxKeys > maxObjectList {
		maxKeys = maxObjectList
	}

	// Default is recursive, if delimiter is set then list non recursive.
	recursive := true
	if delimiter == slashSeparator {
		recursive = false
	}

	// Convert entry to ObjectInfo
	entryToObjectInfo := func(entry string) (objInfo ObjectInfo, err error) {
		if strings.HasSuffix(entry, slashSeparator) {
			// Object name needs to be full path.
			objInfo.Name = entry
			objInfo.IsDir = true
			return
		}
		// Stat the file to get file size.
		var fi os.FileInfo
		fi, err = fsStatFile(pathJoin(fs.fsPath, bucket, entry))
		if err != nil {
			return ObjectInfo{}, toObjectErr(traceError(err), bucket, entry)
		}
		fsMeta := fsMetaV1{}
		return fsMeta.ToObjectInfo(bucket, entry, fi), nil
	}

	heal := false // true only for xl.ListObjectsHeal()
	walkResultCh, endWalkCh := fs.listPool.Release(listParams{bucket, recursive, marker, prefix, heal})
	if walkResultCh == nil {
		endWalkCh = make(chan struct{})
		isLeaf := func(bucket, object string) bool {
			// bucket argument is unused as we don't need to StatFile
			// to figure if it's a file, just need to check that the
			// object string does not end with "/".
			return !strings.HasSuffix(object, slashSeparator)
		}
		listDir := fs.listDirFactory(isLeaf)
		walkResultCh = startTreeWalk(bucket, prefix, marker, recursive, listDir, isLeaf, endWalkCh)
	}

	var objInfos []ObjectInfo
	var eof bool
	var nextMarker string

	// List until maxKeys requested.
	for i := 0; i < maxKeys; {
		walkResult, ok := <-walkResultCh
		if !ok {
			// Closed channel.
			eof = true
			break
		}
		// For any walk error return right away.
		if walkResult.err != nil {
			// File not found is a valid case.
			if errorCause(walkResult.err) == errFileNotFound {
				return ListObjectsInfo{}, nil
			}
			return ListObjectsInfo{}, toObjectErr(walkResult.err, bucket, prefix)
		}
		objInfo, err := entryToObjectInfo(walkResult.entry)
		if err != nil {
			return ListObjectsInfo{}, nil
		}
		nextMarker = objInfo.Name
		objInfos = append(objInfos, objInfo)
		if walkResult.end {
			eof = true
			break
		}
		i++
	}

	// Save list routine for the next marker if we haven't reached EOF.
	params := listParams{bucket, recursive, nextMarker, prefix, heal}
	if !eof {
		fs.listPool.Set(params, walkResultCh, endWalkCh)
	}

	result := ListObjectsInfo{IsTruncated: !eof}
	for _, objInfo := range objInfos {
		result.NextMarker = objInfo.Name
		if objInfo.IsDir {
			result.Prefixes = append(result.Prefixes, objInfo.Name)
			continue
		}
		result.Objects = append(result.Objects, objInfo)
	}

	// Success.
	return result, nil
}

// HealObject - no-op for fs. Valid only for XL.
func (fs fsObjects) HealObject(bucket, object string) error {
	return traceError(NotImplemented{})
}

// HealBucket - no-op for fs, Valid only for XL.
func (fs fsObjects) HealBucket(bucket string) error {
	return traceError(NotImplemented{})
}

// ListObjectsHeal - list all objects to be healed. Valid only for XL
func (fs fsObjects) ListObjectsHeal(bucket, prefix, marker, delimiter string, maxKeys int) (ListObjectsInfo, error) {
	return ListObjectsInfo{}, traceError(NotImplemented{})
}

// ListBucketsHeal - list all buckets to be healed. Valid only for XL
func (fs fsObjects) ListBucketsHeal() ([]BucketInfo, error) {
	return []BucketInfo{}, traceError(NotImplemented{})
}
