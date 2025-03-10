/*
 * MinIO Cloud Storage, (C) 2019-2020 MinIO, Inc.
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
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"sort"

	"github.com/gorilla/mux"
	"github.com/minio/minio/cmd/config/dns"
	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/auth"
	iampolicy "github.com/minio/minio/pkg/iam/policy"
	"github.com/minio/minio/pkg/madmin"
)

func validateAdminUsersReq(ctx context.Context, w http.ResponseWriter, r *http.Request, action iampolicy.AdminAction) (ObjectLayer, auth.Credentials) {
	var cred auth.Credentials
	var adminAPIErr APIErrorCode

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return nil, cred
	}

	// Validate request signature.
	cred, adminAPIErr = checkAdminRequestAuth(ctx, r, action, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(adminAPIErr), r.URL)
		return nil, cred
	}

	return objectAPI, cred
}

// RemoveUser - DELETE /minio/admin/v3/remove-user?accessKey=<access_key>
func (a adminAPIHandlers) RemoveUser(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "RemoveUser")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.DeleteUserAdminAction)
	if objectAPI == nil {
		return
	}

	vars := mux.Vars(r)
	accessKey := vars["accessKey"]

	ok, _, err := globalIAMSys.IsTempUser(accessKey)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}
	if ok {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, errIAMActionNotAllowed), r.URL)
		return
	}

	if err := globalIAMSys.DeleteUser(accessKey); err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Notify all other MinIO peers to delete user.
	for _, nerr := range globalNotificationSys.DeleteUser(accessKey) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// ListUsers - GET /minio/admin/v3/list-users
func (a adminAPIHandlers) ListUsers(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ListUsers")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, cred := validateAdminUsersReq(ctx, w, r, iampolicy.ListUsersAdminAction)
	if objectAPI == nil {
		return
	}

	password := cred.SecretKey

	allCredentials, err := globalIAMSys.ListUsers()
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	data, err := json.Marshal(allCredentials)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	econfigData, err := madmin.EncryptData(password, data)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, econfigData)
}

// GetUserInfo - GET /minio/admin/v3/user-info
func (a adminAPIHandlers) GetUserInfo(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "GetUserInfo")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	name := vars["accessKey"]

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	cred, claims, owner, s3Err := validateAdminSignature(ctx, r, "")
	if s3Err != ErrNone {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	accessKey := cred.AccessKey
	if cred.ParentUser != "" {
		accessKey = cred.ParentUser
	}

	implicitPerm := name == accessKey
	if !implicitPerm {
		if !globalIAMSys.IsAllowed(iampolicy.Args{
			AccountName:     accessKey,
			Groups:          cred.Groups,
			Action:          iampolicy.GetUserAdminAction,
			ConditionValues: getConditionValues(r, "", accessKey, claims),
			IsOwner:         owner,
			Claims:          claims,
		}) {
			writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
			return
		}
	}

	userInfo, err := globalIAMSys.GetUserInfo(name)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	data, err := json.Marshal(userInfo)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, data)
}

// UpdateGroupMembers - PUT /minio/admin/v3/update-group-members
func (a adminAPIHandlers) UpdateGroupMembers(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "UpdateGroupMembers")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.AddUserToGroupAdminAction)
	if objectAPI == nil {
		return
	}

	defer r.Body.Close()
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrInvalidRequest), r.URL)
		return
	}

	var updReq madmin.GroupAddRemove
	err = json.Unmarshal(data, &updReq)
	if err != nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrInvalidRequest), r.URL)
		return
	}

	if updReq.IsRemove {
		err = globalIAMSys.RemoveUsersFromGroup(updReq.Group, updReq.Members)
	} else {
		err = globalIAMSys.AddUsersToGroup(updReq.Group, updReq.Members)
	}

	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Notify all other MinIO peers to load group.
	for _, nerr := range globalNotificationSys.LoadGroup(updReq.Group) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// GetGroup - /minio/admin/v3/group?group=mygroup1
func (a adminAPIHandlers) GetGroup(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "GetGroup")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.GetGroupAdminAction)
	if objectAPI == nil {
		return
	}

	vars := mux.Vars(r)
	group := vars["group"]

	gdesc, err := globalIAMSys.GetGroupDescription(group)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	body, err := json.Marshal(gdesc)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, body)
}

// ListGroups - GET /minio/admin/v3/groups
func (a adminAPIHandlers) ListGroups(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ListGroups")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.ListGroupsAdminAction)
	if objectAPI == nil {
		return
	}

	groups, err := globalIAMSys.ListGroups()
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	body, err := json.Marshal(groups)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, body)
}

// SetGroupStatus - PUT /minio/admin/v3/set-group-status?group=mygroup1&status=enabled
func (a adminAPIHandlers) SetGroupStatus(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "SetGroupStatus")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.EnableGroupAdminAction)
	if objectAPI == nil {
		return
	}

	vars := mux.Vars(r)
	group := vars["group"]
	status := vars["status"]

	var err error
	if status == statusEnabled {
		err = globalIAMSys.SetGroupStatus(group, true)
	} else if status == statusDisabled {
		err = globalIAMSys.SetGroupStatus(group, false)
	} else {
		err = errInvalidArgument
	}
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Notify all other MinIO peers to reload user.
	for _, nerr := range globalNotificationSys.LoadGroup(group) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// SetUserStatus - PUT /minio/admin/v3/set-user-status?accessKey=<access_key>&status=[enabled|disabled]
func (a adminAPIHandlers) SetUserStatus(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "SetUserStatus")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.EnableUserAdminAction)
	if objectAPI == nil {
		return
	}

	vars := mux.Vars(r)
	accessKey := vars["accessKey"]
	status := vars["status"]

	// This API is not allowed to lookup accessKey user status
	if accessKey == globalActiveCred.AccessKey {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrInvalidRequest), r.URL)
		return
	}

	if err := globalIAMSys.SetUserStatus(accessKey, madmin.AccountStatus(status)); err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Notify all other MinIO peers to reload user.
	for _, nerr := range globalNotificationSys.LoadUser(accessKey, false) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// AddUser - PUT /minio/admin/v3/add-user?accessKey=<access_key>
func (a adminAPIHandlers) AddUser(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "AddUser")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	accessKey := vars["accessKey"]

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	cred, claims, owner, s3Err := validateAdminSignature(ctx, r, "")
	if s3Err != ErrNone {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	// Not allowed to add a user with same access key as root credential
	if owner && accessKey == cred.AccessKey {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAddUserInvalidArgument), r.URL)
		return
	}

	if (cred.IsTemp() || cred.IsServiceAccount()) && cred.ParentUser == accessKey {
		// Incoming access key matches parent user then we should
		// reject password change requests.
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAddUserInvalidArgument), r.URL)
		return
	}

	implicitPerm := accessKey == cred.AccessKey
	if !implicitPerm {
		parentUser := cred.ParentUser
		if parentUser == "" {
			parentUser = cred.AccessKey
		}
		if !globalIAMSys.IsAllowed(iampolicy.Args{
			AccountName:     parentUser,
			Groups:          cred.Groups,
			Action:          iampolicy.CreateUserAdminAction,
			ConditionValues: getConditionValues(r, "", parentUser, claims),
			IsOwner:         owner,
			Claims:          claims,
		}) {
			writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
			return
		}
	}

	if implicitPerm && !globalIAMSys.IsAllowed(iampolicy.Args{
		AccountName:     accessKey,
		Groups:          cred.Groups,
		Action:          iampolicy.CreateUserAdminAction,
		ConditionValues: getConditionValues(r, "", accessKey, claims),
		IsOwner:         owner,
		Claims:          claims,
		DenyOnly:        true, // check if changing password is explicitly denied.
	}) {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
		return
	}

	if r.ContentLength > maxEConfigJSONSize || r.ContentLength == -1 {
		// More than maxConfigSize bytes were available
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminConfigTooLarge), r.URL)
		return
	}

	password := cred.SecretKey
	configBytes, err := madmin.DecryptData(password, io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminConfigBadJSON), r.URL)
		return
	}

	var uinfo madmin.UserInfo
	if err = json.Unmarshal(configBytes, &uinfo); err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminConfigBadJSON), r.URL)
		return
	}

	if err = globalIAMSys.CreateUser(accessKey, uinfo); err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Notify all other Minio peers to reload user
	for _, nerr := range globalNotificationSys.LoadUser(accessKey, false) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// AddServiceAccount - PUT /minio/admin/v3/add-service-account
func (a adminAPIHandlers) AddServiceAccount(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "AddServiceAccount")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	cred, claims, owner, s3Err := validateAdminSignature(ctx, r, "")
	if s3Err != ErrNone {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	password := cred.SecretKey
	reqBytes, err := madmin.DecryptData(password, io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErrWithErr(ErrAdminConfigBadJSON, err), r.URL)
		return
	}

	var createReq madmin.AddServiceAccountReq
	if err = json.Unmarshal(reqBytes, &createReq); err != nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErrWithErr(ErrAdminConfigBadJSON, err), r.URL)
		return
	}

	// Disallow creating service accounts by root user.
	if owner {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminAccountNotEligible), r.URL)
		return
	}

	// Disallow creating service accounts for root user.
	if createReq.TargetUser == globalActiveCred.AccessKey {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminAccountNotEligible), r.URL)
		return
	}

	var (
		targetUser   string
		targetGroups []string
	)

	targetUser = createReq.TargetUser

	// Need permission if we are creating a service acccount
	// for a user <> to the request sender
	if targetUser != "" && targetUser != cred.AccessKey {
		if !globalIAMSys.IsAllowed(iampolicy.Args{
			AccountName:     cred.AccessKey,
			Action:          iampolicy.CreateServiceAccountAdminAction,
			ConditionValues: getConditionValues(r, "", cred.AccessKey, claims),
			IsOwner:         owner,
			Claims:          claims,
		}) {
			writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
			return
		}
	}

	if globalLDAPConfig.Enabled && targetUser != "" {
		// If LDAP enabled, service accounts need
		// to be created only for LDAP users.
		var err error
		_, targetGroups, err = globalLDAPConfig.LookupUserDN(targetUser)
		if err != nil {
			writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
			return
		}
	} else {
		if targetUser == "" {
			targetUser = cred.AccessKey
		}
		if cred.ParentUser != "" {
			targetUser = cred.ParentUser
		}
		targetGroups = cred.Groups
	}

	opts := newServiceAccountOpts{sessionPolicy: createReq.Policy, accessKey: createReq.AccessKey, secretKey: createReq.SecretKey}
	newCred, err := globalIAMSys.NewServiceAccount(ctx, targetUser, targetGroups, opts)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Notify all other Minio peers to reload user the service account
	for _, nerr := range globalNotificationSys.LoadServiceAccount(newCred.AccessKey) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}

	var createResp = madmin.AddServiceAccountResp{
		Credentials: auth.Credentials{
			AccessKey: newCred.AccessKey,
			SecretKey: newCred.SecretKey,
		},
	}

	data, err := json.Marshal(createResp)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	encryptedData, err := madmin.EncryptData(password, data)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, encryptedData)
}

// UpdateServiceAccount - POST /minio/admin/v3/update-service-account
func (a adminAPIHandlers) UpdateServiceAccount(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "UpdateServiceAccount")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	cred, claims, owner, s3Err := validateAdminSignature(ctx, r, "")
	if s3Err != ErrNone {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	accessKey := mux.Vars(r)["accessKey"]
	if accessKey == "" {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrInvalidRequest), r.URL)
		return
	}

	// Disallow editing service accounts by root user.
	if owner {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminAccountNotEligible), r.URL)
		return
	}

	svcAccount, _, err := globalIAMSys.GetServiceAccount(ctx, accessKey)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	if !globalIAMSys.IsAllowed(iampolicy.Args{
		AccountName:     cred.AccessKey,
		Action:          iampolicy.UpdateServiceAccountAdminAction,
		ConditionValues: getConditionValues(r, "", cred.AccessKey, claims),
		IsOwner:         owner,
		Claims:          claims,
	}) {
		requestUser := cred.AccessKey
		if cred.ParentUser != "" {
			requestUser = cred.ParentUser
		}

		if requestUser != svcAccount.ParentUser {
			writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
			return
		}
	}

	password := cred.SecretKey
	reqBytes, err := madmin.DecryptData(password, io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErrWithErr(ErrAdminConfigBadJSON, err), r.URL)
		return
	}

	var updateReq madmin.UpdateServiceAccountReq
	if err = json.Unmarshal(reqBytes, &updateReq); err != nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErrWithErr(ErrAdminConfigBadJSON, err), r.URL)
		return
	}

	opts := updateServiceAccountOpts{sessionPolicy: updateReq.NewPolicy, secretKey: updateReq.NewSecretKey, status: updateReq.NewStatus}
	err = globalIAMSys.UpdateServiceAccount(ctx, accessKey, opts)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Notify all other Minio peers to reload user the service account
	for _, nerr := range globalNotificationSys.LoadServiceAccount(accessKey) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}

	writeSuccessNoContent(w)
}

// InfoServiceAccount - GET /minio/admin/v3/info-service-account
func (a adminAPIHandlers) InfoServiceAccount(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "InfoServiceAccount")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	cred, claims, owner, s3Err := validateAdminSignature(ctx, r, "")
	if s3Err != ErrNone {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	// Disallow creating service accounts by root user.
	if owner {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminAccountNotEligible), r.URL)
		return
	}

	accessKey := mux.Vars(r)["accessKey"]
	if accessKey == "" {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrInvalidRequest), r.URL)
		return
	}

	svcAccount, policy, err := globalIAMSys.GetServiceAccount(ctx, accessKey)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	if !globalIAMSys.IsAllowed(iampolicy.Args{
		AccountName:     cred.AccessKey,
		Action:          iampolicy.ListServiceAccountsAdminAction,
		ConditionValues: getConditionValues(r, "", cred.AccessKey, claims),
		IsOwner:         owner,
		Claims:          claims,
	}) {
		requestUser := cred.AccessKey
		if cred.ParentUser != "" {
			requestUser = cred.ParentUser
		}

		if requestUser != svcAccount.ParentUser {
			writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
			return
		}
	}

	var svcAccountPolicy iampolicy.Policy

	impliedPolicy := policy == nil

	// If policy is empty, check for policy of the parent user
	if !impliedPolicy {
		svcAccountPolicy = svcAccountPolicy.Merge(*policy)
	} else {
		policiesNames, err := globalIAMSys.PolicyDBGet(svcAccount.AccessKey, false)
		if err != nil {
			writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
			return
		}
		svcAccountPolicy = svcAccountPolicy.Merge(globalIAMSys.GetCombinedPolicy(policiesNames...))
	}

	policyJSON, err := json.Marshal(svcAccountPolicy)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	var infoResp = madmin.InfoServiceAccountResp{
		ParentUser:    svcAccount.ParentUser,
		AccountStatus: svcAccount.Status,
		ImpliedPolicy: impliedPolicy,
		Policy:        string(policyJSON),
	}

	data, err := json.Marshal(infoResp)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	encryptedData, err := madmin.EncryptData(cred.SecretKey, data)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, encryptedData)
}

// ListServiceAccounts - GET /minio/admin/v3/list-service-accounts
func (a adminAPIHandlers) ListServiceAccounts(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ListServiceAccounts")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	cred, claims, owner, s3Err := validateAdminSignature(ctx, r, "")
	if s3Err != ErrNone {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	// Disallow creating service accounts by root user.
	if owner {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminAccountNotEligible), r.URL)
		return
	}

	var targetAccount string

	user := r.URL.Query().Get("user")
	if user != "" {
		if !globalIAMSys.IsAllowed(iampolicy.Args{
			AccountName:     cred.AccessKey,
			Action:          iampolicy.ListServiceAccountsAdminAction,
			ConditionValues: getConditionValues(r, "", cred.AccessKey, claims),
			IsOwner:         owner,
			Claims:          claims,
		}) {
			writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
			return
		}
		targetAccount = user
	} else {
		targetAccount = cred.AccessKey
		if cred.ParentUser != "" {
			targetAccount = cred.ParentUser
		}
	}

	serviceAccounts, err := globalIAMSys.ListServiceAccounts(ctx, targetAccount)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	var serviceAccountsNames []string

	for _, svc := range serviceAccounts {
		serviceAccountsNames = append(serviceAccountsNames, svc.AccessKey)
	}

	var listResp = madmin.ListServiceAccountsResp{
		Accounts: serviceAccountsNames,
	}

	data, err := json.Marshal(listResp)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	encryptedData, err := madmin.EncryptData(cred.SecretKey, data)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, encryptedData)
}

// DeleteServiceAccount - DELETE /minio/admin/v3/delete-service-account
func (a adminAPIHandlers) DeleteServiceAccount(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "DeleteServiceAccount")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	cred, claims, owner, s3Err := validateAdminSignature(ctx, r, "")
	if s3Err != ErrNone {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	// Disallow creating service accounts by root user.
	if owner {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminAccountNotEligible), r.URL)
		return
	}

	serviceAccount := mux.Vars(r)["accessKey"]
	if serviceAccount == "" {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminInvalidArgument), r.URL)
		return
	}

	svcAccount, _, err := globalIAMSys.GetServiceAccount(ctx, serviceAccount)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	adminPrivilege := globalIAMSys.IsAllowed(iampolicy.Args{
		AccountName:     cred.AccessKey,
		Action:          iampolicy.RemoveServiceAccountAdminAction,
		ConditionValues: getConditionValues(r, "", cred.AccessKey, claims),
		IsOwner:         owner,
		Claims:          claims,
	})

	if !adminPrivilege {
		parentUser := cred.AccessKey
		if cred.ParentUser != "" {
			parentUser = cred.ParentUser
		}
		if parentUser != svcAccount.ParentUser {
			// The service account belongs to another user but return not
			// found error to mitigate brute force attacks. or the
			// serviceAccount doesn't exist.
			writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAdminServiceAccountNotFound), r.URL)
			return
		}
	}

	err = globalIAMSys.DeleteServiceAccount(ctx, serviceAccount)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	writeSuccessNoContent(w)
}

// AccountInfoHandler returns usage
func (a adminAPIHandlers) AccountInfoHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "AccountInfo")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	cred, claims, owner, s3Err := validateAdminSignature(ctx, r, "")
	if s3Err != ErrNone {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	// Set prefix value for "s3:prefix" policy conditionals.
	r.Header.Set("prefix", "")

	// Set delimiter value for "s3:delimiter" policy conditionals.
	r.Header.Set("delimiter", SlashSeparator)

	isAllowedAccess := func(bucketName string) (rd, wr bool) {
		// Use the following trick to filter in place
		// https://github.com/golang/go/wiki/SliceTricks#filter-in-place
		if globalIAMSys.IsAllowed(iampolicy.Args{
			AccountName:     cred.AccessKey,
			Groups:          cred.Groups,
			Action:          iampolicy.ListBucketAction,
			BucketName:      bucketName,
			ConditionValues: getConditionValues(r, "", cred.AccessKey, claims),
			IsOwner:         owner,
			ObjectName:      "",
			Claims:          claims,
		}) {
			rd = true
		}

		if globalIAMSys.IsAllowed(iampolicy.Args{
			AccountName:     cred.AccessKey,
			Groups:          cred.Groups,
			Action:          iampolicy.PutObjectAction,
			BucketName:      bucketName,
			ConditionValues: getConditionValues(r, "", cred.AccessKey, claims),
			IsOwner:         owner,
			ObjectName:      "",
			Claims:          claims,
		}) {
			wr = true
		}

		return rd, wr
	}

	// Load the latest calculated data usage
	dataUsageInfo, err := loadDataUsageFromBackend(ctx, objectAPI)
	if err != nil {
		// log the error, continue with the accounting response
		logger.LogIf(ctx, err)
	}

	// If etcd, dns federation configured list buckets from etcd.
	var buckets []BucketInfo
	if globalDNSConfig != nil && globalBucketFederation {
		dnsBuckets, err := globalDNSConfig.List()
		if err != nil && !IsErrIgnored(err,
			dns.ErrNoEntriesFound,
			dns.ErrDomainMissing) {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL, guessIsBrowserReq(r))
			return
		}
		for _, dnsRecords := range dnsBuckets {
			buckets = append(buckets, BucketInfo{
				Name:    dnsRecords[0].Key,
				Created: dnsRecords[0].CreationDate,
			})
		}
		sort.Slice(buckets, func(i, j int) bool {
			return buckets[i].Name < buckets[j].Name
		})
	} else {
		buckets, err = objectAPI.ListBuckets(ctx)
		if err != nil {
			writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
			return
		}
	}

	accountName := cred.AccessKey
	var policies []string
	switch globalIAMSys.usersSysType {
	case MinIOUsersSysType:
		policies, err = globalIAMSys.PolicyDBGet(accountName, false)
	case LDAPUsersSysType:
		parentUser := accountName
		if cred.ParentUser != "" {
			parentUser = cred.ParentUser
		}
		policies, err = globalIAMSys.PolicyDBGet(parentUser, false, cred.Groups...)
	default:
		err = errors.New("should not happen!")
	}
	if err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	acctInfo := madmin.AccountInfo{
		AccountName: accountName,
		Policy:      globalIAMSys.GetCombinedPolicy(policies...),
	}

	for _, bucket := range buckets {
		rd, wr := isAllowedAccess(bucket.Name)
		if rd || wr {
			var size uint64
			// Fetch the data usage of the current bucket
			if !dataUsageInfo.LastUpdate.IsZero() {
				size = dataUsageInfo.BucketsUsage[bucket.Name].Size
			}
			acctInfo.Buckets = append(acctInfo.Buckets, madmin.BucketAccessInfo{
				Name:    bucket.Name,
				Created: bucket.Created,
				Size:    size,
				Access: madmin.AccountAccess{
					Read:  rd,
					Write: wr,
				},
			})
		}
	}

	usageInfoJSON, err := json.Marshal(acctInfo)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, usageInfoJSON)
}

// InfoCannedPolicyV2 - GET /minio/admin/v2/info-canned-policy?name={policyName}
func (a adminAPIHandlers) InfoCannedPolicyV2(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "InfoCannedPolicyV2")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.GetPolicyAdminAction)
	if objectAPI == nil {
		return
	}

	policy, err := globalIAMSys.InfoPolicy(mux.Vars(r)["name"])
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	data, err := json.Marshal(policy)
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	w.Write(data)
	w.(http.Flusher).Flush()
}

// InfoCannedPolicy - GET /minio/admin/v3/info-canned-policy?name={policyName}
func (a adminAPIHandlers) InfoCannedPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "InfoCannedPolicy")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.GetPolicyAdminAction)
	if objectAPI == nil {
		return
	}

	policy, err := globalIAMSys.InfoPolicy(mux.Vars(r)["name"])
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	if err = json.NewEncoder(w).Encode(policy); err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}
	w.(http.Flusher).Flush()
}

// ListCannedPoliciesV2 - GET /minio/admin/v2/list-canned-policies
func (a adminAPIHandlers) ListCannedPoliciesV2(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ListCannedPoliciesV2")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.ListUserPoliciesAdminAction)
	if objectAPI == nil {
		return
	}

	policies, err := globalIAMSys.ListPolicies()
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	policyMap := make(map[string][]byte, len(policies))
	for k, p := range policies {
		var err error
		policyMap[k], err = json.Marshal(p)
		if err != nil {
			logger.LogIf(ctx, err)
			continue
		}
	}
	if err = json.NewEncoder(w).Encode(policyMap); err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	w.(http.Flusher).Flush()
}

// ListCannedPolicies - GET /minio/admin/v3/list-canned-policies
func (a adminAPIHandlers) ListCannedPolicies(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ListCannedPolicies")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.ListUserPoliciesAdminAction)
	if objectAPI == nil {
		return
	}

	policies, err := globalIAMSys.ListPolicies()
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	var newPolicies = make(map[string]iampolicy.Policy)
	for name, p := range policies {
		_, err = json.Marshal(p)
		if err != nil {
			logger.LogIf(ctx, err)
			continue
		}
		newPolicies[name] = p
	}
	if err = json.NewEncoder(w).Encode(newPolicies); err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	w.(http.Flusher).Flush()
}

// RemoveCannedPolicy - DELETE /minio/admin/v3/remove-canned-policy?name=<policy_name>
func (a adminAPIHandlers) RemoveCannedPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "RemoveCannedPolicy")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.DeletePolicyAdminAction)
	if objectAPI == nil {
		return
	}

	vars := mux.Vars(r)
	policyName := vars["name"]

	if err := globalIAMSys.DeletePolicy(policyName); err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Notify all other MinIO peers to delete policy
	for _, nerr := range globalNotificationSys.DeletePolicy(policyName) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// AddCannedPolicy - PUT /minio/admin/v3/add-canned-policy?name=<policy_name>
func (a adminAPIHandlers) AddCannedPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "AddCannedPolicy")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.CreatePolicyAdminAction)
	if objectAPI == nil {
		return
	}

	vars := mux.Vars(r)
	policyName := vars["name"]

	// Error out if Content-Length is missing.
	if r.ContentLength <= 0 {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrMissingContentLength), r.URL)
		return
	}

	// Error out if Content-Length is beyond allowed size.
	if r.ContentLength > maxBucketPolicySize {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrEntityTooLarge), r.URL)
		return
	}

	iamPolicy, err := iampolicy.ParseConfig(io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Version in policy must not be empty
	if iamPolicy.Version == "" {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrMalformedPolicy), r.URL)
		return
	}

	if err = globalIAMSys.SetPolicy(policyName, *iamPolicy); err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Notify all other MinIO peers to reload policy
	for _, nerr := range globalNotificationSys.LoadPolicy(policyName) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// SetPolicyForUserOrGroup - PUT /minio/admin/v3/set-policy?policy=xxx&user-or-group=?[&is-group]
func (a adminAPIHandlers) SetPolicyForUserOrGroup(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "SetPolicyForUserOrGroup")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI, _ := validateAdminUsersReq(ctx, w, r, iampolicy.AttachPolicyAdminAction)
	if objectAPI == nil {
		return
	}

	vars := mux.Vars(r)
	policyName := vars["policyName"]
	entityName := vars["userOrGroup"]
	isGroup := vars["isGroup"] == "true"

	if !isGroup {
		ok, _, err := globalIAMSys.IsTempUser(entityName)
		if err != nil && err != errNoSuchUser {
			writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
			return
		}
		if ok {
			writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, errIAMActionNotAllowed), r.URL)
			return
		}
	}

	if err := globalIAMSys.PolicyDBSet(entityName, policyName, isGroup); err != nil {
		writeErrorResponseJSON(ctx, w, toAdminAPIErr(ctx, err), r.URL)
		return
	}

	// Notify all other MinIO peers to reload policy
	for _, nerr := range globalNotificationSys.LoadPolicyMapping(entityName, isGroup) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}
