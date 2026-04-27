package _189pc

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/go-resty/resty/v2"
	"github.com/google/uuid"
)

const casTempDirName = "TEMP"

type casUploadInfo struct {
	Name     string
	Size     int64
	MD5      string
	SliceMD5 string
}

type casPayload struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	MD5        string `json:"md5"`
	SliceMD5   string `json:"sliceMd5"`
	CreateTime string `json:"create_time"`
}

func isCASName(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".cas")
}

func (y *Cloud189PC) shouldUploadCAS(name string) bool {
	return y.GenerateCAS && !isCASName(name)
}

func (y *Cloud189PC) shouldDeleteSource() bool {
	return y.GenerateCAS && y.DeleteSource
}

func (y *Cloud189PC) uploadCAS(ctx context.Context, dstDir model.Obj, info *casUploadInfo) (model.Obj, error) {
	if info == nil || !y.shouldUploadCAS(info.Name) {
		return nil, nil
	}
	content, err := utils.Json.Marshal(casPayload{
		Name:       info.Name,
		Size:       info.Size,
		MD5:        info.MD5,
		SliceMD5:   info.SliceMD5,
		CreateTime: strconv.FormatInt(time.Now().Unix(), 10),
	})
	if err != nil {
		return nil, err
	}
	content = []byte(base64.StdEncoding.EncodeToString(content))

	now := time.Now()
	casObj := &model.Object{
		Name:     info.Name + ".cas",
		Size:     int64(len(content)),
		Modified: now,
		Ctime:    now,
		HashInfo: utils.NewHashInfo(utils.MD5, utils.HashData(utils.MD5, content)),
	}
	casStream := &stream.FileStream{
		Ctx:      ctx,
		Obj:      casObj,
		Reader:   bytes.NewReader(content),
		Mimetype: "text/plain",
	}
	uploadedCASObj, _, err := y.uploadFile(ctx, dstDir, casStream, func(float64) {})
	if err != nil {
		return nil, err
	}
	if uploadedCASObj != nil {
		return uploadedCASObj, nil
	}
	return casObj, nil
}

func (y *Cloud189PC) deleteSource(ctx context.Context, dstDir model.Obj, uploadedObj model.Obj, info *casUploadInfo) error {
	if info == nil || !y.shouldDeleteSource() || !y.shouldUploadCAS(info.Name) {
		return nil
	}
	if uploadedObj == nil {
		var err error
		uploadedObj, err = y.findFileByName(ctx, info.Name, dstDir.GetID(), y.isFamily())
		if err != nil {
			return err
		}
	}
	return y.Delete(ctx, IF(y.isFamily(), y.FamilyID, ""), uploadedObj)
}

func (y *Cloud189PC) parseCAS(data []byte) (*casUploadInfo, error) {
	data = bytes.TrimSpace(data)
	decoded, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, err
	}
	var payload casPayload
	if err = utils.Json.Unmarshal(decoded, &payload); err != nil {
		return nil, err
	}
	if payload.Name == "" || payload.Size < 0 || payload.MD5 == "" {
		return nil, fmt.Errorf("invalid cas payload")
	}
	if payload.SliceMD5 == "" {
		payload.SliceMD5 = payload.MD5
	}
	return &casUploadInfo{
		Name:     payload.Name,
		Size:     payload.Size,
		MD5:      payload.MD5,
		SliceMD5: payload.SliceMD5,
	}, nil
}

func (y *Cloud189PC) parseCASFromStreamer(file model.FileStreamer) (*casUploadInfo, error) {
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return y.parseCAS(data)
}

func (y *Cloud189PC) parseCASFromObj(ctx context.Context, file model.Obj) (*casUploadInfo, error) {
	link, err := y.Link(ctx, file, model.LinkArgs{Type: "raw_cas"})
	if err != nil {
		return nil, err
	}
	defer link.Close()
	if link.URL == "" {
		return nil, fmt.Errorf("cas link has no url")
	}
	req := base.RestyClient.R().SetContext(ctx)
	if link.Header != nil {
		req.SetHeaders(headerToMap(link.Header))
	}
	resp, err := req.Get(link.URL)
	if err != nil {
		return nil, err
	}
	return y.parseCAS(resp.Body())
}

func headerToMap(header http.Header) map[string]string {
	headers := make(map[string]string, len(header))
	for key, values := range header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}
	return headers
}

func deriveCASRestoreName(casName, originalName string) string {
	baseName := strings.TrimSuffix(casName, path.Ext(casName))
	baseName = strings.TrimSuffix(baseName, path.Ext(baseName))
	ext := path.Ext(originalName)
	if baseName == "" {
		baseName = strings.TrimSuffix(originalName, ext)
	}
	return baseName + ext
}

func resolveCASRestoreName(casName string, info *casUploadInfo) (string, error) {
	if info == nil {
		return "", fmt.Errorf("cas restore failed: missing cas payload")
	}
	if !isCASName(casName) {
		return "", fmt.Errorf("cas restore failed: current file name %q does not end with .cas", casName)
	}
	trimmedName := strings.TrimSpace(strings.TrimSuffix(casName, path.Ext(casName)))
	if trimmedName == "" {
		return "", fmt.Errorf("cas restore failed: current .cas file name %q has an empty source file name", casName)
	}
	restoreName := strings.TrimSpace(deriveCASRestoreName(casName, info.Name))
	if restoreName == "" {
		return "", fmt.Errorf("cas restore failed: current .cas file name %q has an empty source file name", casName)
	}
	if strings.ContainsAny(restoreName, `/\`) {
		return "", fmt.Errorf("cas restore failed: source file name %q contains a path", restoreName)
	}
	return restoreName, nil
}

func (y *Cloud189PC) restoreCAS(ctx context.Context, dstDir model.Obj, info *casUploadInfo, casName string, temp bool) (model.Obj, error) {
	targetName, err := resolveCASRestoreName(casName, info)
	if err != nil {
		return nil, err
	}
	if temp {
		targetName = fmt.Sprintf("TEMP_%d_%s_%s", time.Now().UnixNano()/1e6, uuid.NewString()[:5], targetName)
	}
	if existing, err := y.findFileByName(ctx, targetName, dstDir.GetID(), y.isFamily()); err == nil && !temp {
		return existing, nil
	}
	restoreInfo := *info
	restoreInfo.Name = targetName

	if temp && !y.isFamily() && y.FamilyTransfer {
		return y.restoreCASDirect(ctx, dstDir, &restoreInfo, true, false)
	}
	if !y.isFamily() && y.FamilyTransfer {
		y.beginCleanupTask()
		defer y.endCleanupTask()
		if err := y.ensureFamilyTransferFolder(ctx); err != nil {
			return nil, err
		}
		obj, err := y.restoreCASDirect(ctx, y.familyTransferFolder, &restoreInfo, true, false)
		if err != nil {
			return nil, err
		}
		if err = y.SaveFamilyFileToPersonCloud(ctx, y.FamilyID, obj, dstDir, true); err != nil {
			y.queueFamilyTransferCleanupObj(obj)
			return nil, err
		}
		y.queueFamilyTransferCleanupObj(obj)
		return y.findFileByName(ctx, targetName, dstDir.GetID(), false)
	}
	return y.restoreCASDirect(ctx, dstDir, &restoreInfo, y.isFamily(), true)
}

func (y *Cloud189PC) restoreCASDirect(ctx context.Context, dstDir model.Obj, info *casUploadInfo, isFamily bool, overwrite bool) (model.Obj, error) {
	sliceSize := partSize(info.Size)
	fullUrl := UPLOAD_URL
	if isFamily {
		fullUrl += "/family"
	} else {
		fullUrl += "/person"
	}
	params := Params{
		"parentFolderId": dstDir.GetID(),
		"fileName":       url.QueryEscape(info.Name),
		"fileSize":       fmt.Sprint(info.Size),
		"fileMd5":        info.MD5,
		"sliceSize":      fmt.Sprint(sliceSize),
		"sliceMd5":       info.SliceMD5,
	}
	if isFamily {
		params.Set("familyId", y.FamilyID)
	}
	var uploadInfo InitMultiUploadResp
	_, err := y.request(fullUrl+"/initMultiUpload", http.MethodGet, func(req *resty.Request) {
		req.SetContext(ctx)
	}, params, &uploadInfo, isFamily)
	if err != nil {
		return nil, err
	}
	if uploadInfo.Data.FileDataExists != 1 {
		return nil, fmt.Errorf("cas restore failed: source file data does not exist in cloud")
	}
	var resp CommitMultiUploadFileResp
	_, err = y.request(fullUrl+"/commitMultiUploadFile", http.MethodGet, func(req *resty.Request) {
		req.SetContext(ctx)
	}, Params{
		"uploadFileId": uploadInfo.Data.UploadFileID,
		"isLog":        "0",
		"opertype":     IF(overwrite, "3", "1"),
	}, &resp, isFamily)
	if err != nil {
		return nil, err
	}
	return resp.toFile(), nil
}

func (y *Cloud189PC) shouldPlayCAS(file model.Obj, args model.LinkArgs) bool {
	if !isCASName(file.GetName()) {
		return false
	}
	switch strings.ToLower(args.Type) {
	case "video", "preview", "cas_video", "casplay":
		return true
	default:
		return false
	}
}

func (y *Cloud189PC) CASPreviewName(ctx context.Context, file model.Obj) (string, error) {
	if !isCASName(file.GetName()) {
		return file.GetName(), nil
	}
	info, err := y.parseCASFromObj(ctx, file)
	if err != nil {
		return "", err
	}
	previewName, err := resolveCASRestoreName(file.GetName(), info)
	if err != nil {
		return "", err
	}
	if !isVideoName(previewName) {
		return file.GetName(), nil
	}
	return previewName, nil
}

func (y *Cloud189PC) linkCASVideo(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	info, err := y.parseCASFromObj(ctx, file)
	if err != nil {
		return nil, err
	}
	if !isVideoName(info.Name) {
		return y.Link(ctx, file, model.LinkArgs{IP: args.IP, Header: args.Header, Type: "raw_cas", Redirect: args.Redirect})
	}
	y.beginCleanupTask()
	tempDir, err := y.ensureTempDir(ctx)
	if err != nil {
		y.endCleanupTask()
		return nil, err
	}
	tempObj, err := y.restoreCAS(ctx, tempDir, info, file.GetName(), true)
	if err != nil {
		y.endCleanupTask()
		return nil, err
	}
	tempIsFamily := y.isFamily() || y.FamilyTransfer
	link, err := y.linkObj(ctx, tempObj, model.LinkArgs{IP: args.IP, Header: args.Header, Type: "raw_video", Redirect: args.Redirect}, tempIsFamily)
	if err != nil {
		_ = y.Delete(context.TODO(), IF(tempIsFamily, y.FamilyID, ""), tempObj)
		y.endCleanupTask()
		return nil, err
	}
	go func() {
		if err := y.Delete(context.TODO(), IF(tempIsFamily, y.FamilyID, ""), tempObj); err != nil {
			utils.Log.Errorf("casPlayTempDeleteError:%s", err)
		}
		y.endCleanupTask()
	}()
	return link, nil
}

func (y *Cloud189PC) linkObj(ctx context.Context, file model.Obj, args model.LinkArgs, isFamily bool) (*model.Link, error) {
	var downloadUrl struct {
		URL string `json:"fileDownloadUrl"`
	}
	fullUrl := API_URL
	if isFamily {
		fullUrl += "/family/file"
	}
	fullUrl += "/getFileDownloadUrl.action"
	_, err := y.get(fullUrl, func(r *resty.Request) {
		r.SetContext(ctx)
		r.SetQueryParam("fileId", file.GetID())
		if isFamily {
			r.SetQueryParam("familyId", y.FamilyID)
		} else {
			r.SetQueryParams(map[string]string{
				"dt":   "3",
				"flag": "1",
			})
		}
	}, &downloadUrl, isFamily)
	if err != nil {
		return nil, err
	}
	downloadUrl.URL = strings.Replace(strings.ReplaceAll(downloadUrl.URL, "&amp;", "&"), "http://", "https://", 1)
	res, err := base.NoRedirectClient.R().SetContext(ctx).SetDoNotParseResponse(true).Get(downloadUrl.URL)
	if err != nil {
		return nil, err
	}
	defer res.RawBody().Close()
	if res.StatusCode() == 302 {
		downloadUrl.URL = res.Header().Get("location")
	}
	return &model.Link{
		URL: downloadUrl.URL,
		Header: http.Header{
			"User-Agent": []string{base.UserAgent},
		},
	}, nil
}

func isVideoName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".mkv", ".avi", ".mov", ".webm", ".flv", ".ts", ".m2ts", ".wmv", ".rmvb", ".m4v", ".mpg", ".mpeg", ".3gp":
		return true
	default:
		return false
	}
}

func (y *Cloud189PC) ensureTempDir(ctx context.Context) (model.Obj, error) {
	isFamily := y.isFamily()
	if !isFamily && y.FamilyTransfer {
		isFamily = true
	}
	if obj, err := y.findFolderByName(ctx, casTempDirName, IF(isFamily, "", y.RootFolderID), isFamily); err == nil {
		return obj, nil
	}
	fullUrl := API_URL
	if isFamily {
		fullUrl += "/family/file"
	}
	fullUrl += "/createFolder.action"
	var newFolder Cloud189Folder
	_, err := y.post(fullUrl, func(req *resty.Request) {
		req.SetContext(ctx)
		req.SetQueryParams(map[string]string{
			"folderName":   casTempDirName,
			"relativePath": "",
		})
		if isFamily {
			req.SetQueryParams(map[string]string{
				"familyId": y.FamilyID,
				"parentId": "",
			})
		} else {
			req.SetQueryParam("parentFolderId", y.RootFolderID)
		}
	}, &newFolder, isFamily)
	if err != nil {
		return nil, err
	}
	return &newFolder, nil
}

func (y *Cloud189PC) findFolderByName(ctx context.Context, searchName string, folderId string, isFamily bool) (*Cloud189Folder, error) {
	for pageNum := 1; ; pageNum++ {
		resp, err := y.getFilesWithPage(ctx, folderId, isFamily, pageNum, 10, "filename", "asc")
		if err != nil {
			return nil, err
		}
		if resp.FileListAO.Count == 0 {
	return nil, errs.ObjectNotFound
}

func (y *Cloud189PC) beginAutoRestore(path string) bool {
	_, loaded := y.autoRestoreInFlight.LoadOrStore(path, struct{}{})
	return !loaded
}

func (y *Cloud189PC) endAutoRestore(path string) {
	y.autoRestoreInFlight.Delete(path)
}
		for i := 0; i < len(resp.FileListAO.FolderList); i++ {
			folder := resp.FileListAO.FolderList[i]
			if folder.Name == searchName {
				return &folder, nil
			}
		}
	}
}

func (y *Cloud189PC) findFamilyTransferFolder(ctx context.Context) (*Cloud189Folder, error) {
	return y.findFolderByName(ctx, "FamilyTransferFolder", "", true)
}

func (y *Cloud189PC) notifyTaskDone() {
	if y.debounceClean != nil {
		y.debounceClean()
	}
}

func (y *Cloud189PC) beginCleanupTask() {
	y.cleanupMu.Lock()
	y.cleanupActive++
	y.cleanupMu.Unlock()
}

func (y *Cloud189PC) endCleanupTask() {
	y.cleanupMu.Lock()
	if y.cleanupActive > 0 {
		y.cleanupActive--
	}
	y.cleanupMu.Unlock()
	y.notifyTaskDone()
}

func (y *Cloud189PC) queueFamilyTransferCleanupObj(obj model.Obj) {
	if obj == nil {
		return
	}
	y.cleanupMu.Lock()
	y.cleanupFamilyObjs = append(y.cleanupFamilyObjs, obj)
	y.scheduleDebounceCleanLocked()
	y.cleanupMu.Unlock()
}

func (y *Cloud189PC) newDebounceCleaner() func() {
	return func() {
		y.cleanupMu.Lock()
		defer y.cleanupMu.Unlock()
		y.scheduleDebounceCleanLocked()
	}
}

func (y *Cloud189PC) scheduleDebounceCleanLocked() {
	if y.cleanupActive > 0 && len(y.cleanupFamilyObjs) == 0 {
		return
	}
	if y.cleanupTimer != nil {
		y.cleanupTimer.Stop()
	}
	y.cleanupTimer = time.AfterFunc(5*time.Second, y.runDebounceClean)
}

func (y *Cloud189PC) runDebounceClean() {
	y.cleanupMu.Lock()
	familyObjs := y.cleanupFamilyObjs
	y.cleanupFamilyObjs = nil
	shouldCleanAll := y.cleanupActive == 0
	y.cleanupTimer = nil
	y.cleanupMu.Unlock()

	for _, obj := range familyObjs {
		if err := y.deleteFamilyTransferObj(context.TODO(), obj); err != nil {
			utils.Log.Errorf("casFamilyTransferObjCleanError:%s", err)
		}
	}

	if !shouldCleanAll {
		return
	}

	if y.FamilyTransfer && y.familyTransferFolder != nil {
		if err := y.cleanFamilyTransfer(context.TODO()); err != nil {
			utils.Log.Errorf("cleanFamilyTransferFolderError:%s", err)
		}
	}
	if err := y.cleanTempFolder(context.TODO()); err != nil {
		utils.Log.Errorf("casTempCleanError:%s", err)
	}
}

func (y *Cloud189PC) deleteFamilyTransferObj(ctx context.Context, obj model.Obj) error {
	if obj == nil {
		return nil
	}
	target := obj
	if target.GetID() == "" && y.familyTransferFolder != nil && target.GetName() != "" {
		if found, err := y.findFileByName(ctx, target.GetName(), y.familyTransferFolder.GetID(), true); err == nil {
			target = found
		}
	}
	if target.GetID() == "" {
		return nil
	}
	err := y.Delete(ctx, y.FamilyID, target)
	if err != nil && y.familyTransferFolder != nil && obj.GetName() != "" {
		if found, findErr := y.findFileByName(ctx, obj.GetName(), y.familyTransferFolder.GetID(), true); findErr == nil {
			err = y.Delete(ctx, y.FamilyID, found)
		}
	}
	return err
}

func (y *Cloud189PC) cleanTempFolder(ctx context.Context) error {
	isFamily := y.isFamily()
	if !isFamily && y.FamilyTransfer {
		isFamily = true
	}
	tempDir, err := y.findFolderByName(ctx, casTempDirName, IF(isFamily, "", y.RootFolderID), isFamily)
	if err != nil {
		return nil
	}
	files, err := y.getFiles(ctx, tempDir.GetID(), isFamily)
	if err != nil {
		return err
	}
	for _, obj := range files {
		if obj.IsDir() || !strings.HasPrefix(obj.GetName(), "TEMP_") {
			continue
		}
		if err := y.Delete(ctx, IF(isFamily, y.FamilyID, ""), obj); err != nil {
			utils.Log.Errorf("casTempDeleteError:%s", err)
		}
	}
	return nil
}

func (y *Cloud189PC) autoRestoreCAS(ctx context.Context) error {
	if !y.autoRestoreMu.TryLock() {
		return nil
	}
	defer y.autoRestoreMu.Unlock()
	for _, p := range strings.Split(y.AutoRestoreExistingCASPaths, "\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		dir, err := y.getDirByPath(ctx, p, y.isFamily())
		if err != nil {
			utils.Log.Errorf("autoRestoreCASPathError:%s:%s", p, err)
			continue
		}
		if err = y.restoreCASInDir(ctx, dir, utils.FixAndCleanPath(p)); err != nil {
			utils.Log.Errorf("autoRestoreCASDirError:%s:%s", p, err)
		}
	}
	return nil
}

func (y *Cloud189PC) restoreCASInDir(ctx context.Context, dir model.Obj, dirPath string) error {
	files, err := y.getFiles(ctx, dir.GetID(), y.isFamily())
	if err != nil {
		return err
	}
	for _, obj := range files {
		if obj.IsDir() {
			if err := y.restoreCASInDir(ctx, obj, path.Join(dirPath, obj.GetName())); err != nil {
				utils.Log.Errorf("autoRestoreCASSubDirError:%s:%s", obj.GetName(), err)
			}
			continue
		}
		if !isCASName(obj.GetName()) {
			continue
		}
		casPath := path.Join(dirPath, obj.GetName())
		if !y.beginAutoRestore(casPath) {
			continue
		}
		func() {
			defer y.endAutoRestore(casPath)
			info, err := y.parseCASFromObj(ctx, obj)
			if err != nil {
				utils.Log.Errorf("autoRestoreCASParseError:%s:%s", obj.GetName(), err)
				return
			}
			restoredName, err := resolveCASRestoreName(obj.GetName(), info)
			if err != nil {
				utils.Log.Errorf("autoRestoreCASNameError:%s:%s", obj.GetName(), err)
				return
			}
			if _, err = y.findFileByName(ctx, restoredName, dir.GetID(), y.isFamily()); err == nil {
				if y.DeleteCASAfterRestore {
					if err = y.Delete(ctx, IF(y.isFamily(), y.FamilyID, ""), obj); err != nil {
						utils.Log.Errorf("autoRestoreCASDeleteExistingError:%s:%s", obj.GetName(), err)
						return
					}
					op.Cache.DeleteDirectory(y, dirPath)
					y.notifyTaskDone()
				}
				return
			}
			if _, err = y.restoreCAS(ctx, dir, info, obj.GetName(), false); err != nil {
				utils.Log.Errorf("autoRestoreCASError:%s:%s", obj.GetName(), err)
				return
			}
			if y.DeleteCASAfterRestore {
				if err = y.Delete(ctx, IF(y.isFamily(), y.FamilyID, ""), obj); err != nil {
					utils.Log.Errorf("autoRestoreCASDeleteError:%s:%s", obj.GetName(), err)
					return
				}
				op.Cache.DeleteDirectory(y, dirPath)
			}
			y.notifyTaskDone()
		}()
	}
	return nil
}

func (y *Cloud189PC) getDirByPath(ctx context.Context, dirPath string, isFamily bool) (model.Obj, error) {
	dirPath = strings.Trim(dirPath, "/")
	if dirPath == "" {
		return &Cloud189Folder{ID: String(IF(isFamily, "", y.RootFolderID)), Name: "/"}, nil
	}
	current := &Cloud189Folder{ID: String(IF(isFamily, "", y.RootFolderID)), Name: "/"}
	for _, part := range strings.Split(dirPath, "/") {
		if part == "" {
			continue
		}
		children, err := y.getFiles(ctx, current.GetID(), isFamily)
		if err != nil {
			return nil, err
		}
		found := false
		for _, child := range children {
			if child.IsDir() && child.GetName() == part {
				if folder, ok := child.(*Cloud189Folder); ok {
					current = folder
				} else {
					current = &Cloud189Folder{ID: String(child.GetID()), Name: child.GetName()}
				}
				found = true
				break
			}
		}
		if !found {
			return nil, errs.ObjectNotFound
		}
	}
	return current, nil
}
