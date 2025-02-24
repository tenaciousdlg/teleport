/*
Copyright 2023 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package services

import (
	"context"
	"time"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"

	accesslistclient "github.com/gravitational/teleport/api/client/accesslist"
	accesslistv1 "github.com/gravitational/teleport/api/gen/proto/go/teleport/accesslist/v1"
	"github.com/gravitational/teleport/api/types/accesslist"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"
)

var _ AccessLists = (*accesslistclient.Client)(nil)

// AccessListsGetter defines an interface for reading access lists.
type AccessListsGetter interface {
	AccessListMembersGetter

	// GetAccessLists returns a list of all access lists.
	GetAccessLists(context.Context) ([]*accesslist.AccessList, error)
	// ListAccessLists returns a paginated list of access lists.
	ListAccessLists(context.Context, int, string) ([]*accesslist.AccessList, string, error)
	// GetAccessList returns the specified access list resource.
	GetAccessList(context.Context, string) (*accesslist.AccessList, error)
	// GetAccessListsToReview returns access lists that the user needs to review.
	GetAccessListsToReview(context.Context) ([]*accesslist.AccessList, error)
}

// AccessLists defines an interface for managing AccessLists.
type AccessLists interface {
	AccessListsGetter
	AccessListMembers
	AccessListReviews

	// UpsertAccessList creates or updates an access list resource.
	UpsertAccessList(context.Context, *accesslist.AccessList) (*accesslist.AccessList, error)
	// DeleteAccessList removes the specified access list resource.
	DeleteAccessList(context.Context, string) error
	// DeleteAllAccessLists removes all access lists.
	DeleteAllAccessLists(context.Context) error

	// UpsertAccessListWithMembers creates or updates an access list resource and its members.
	UpsertAccessListWithMembers(context.Context, *accesslist.AccessList, []*accesslist.AccessListMember) (*accesslist.AccessList, []*accesslist.AccessListMember, error)

	// AccessRequestPromote promotes an access request to an access list.
	AccessRequestPromote(ctx context.Context, req *accesslistv1.AccessRequestPromoteRequest) (*accesslistv1.AccessRequestPromoteResponse, error)
}

// MarshalAccessList marshals the access list resource to JSON.
func MarshalAccessList(accessList *accesslist.AccessList, opts ...MarshalOption) ([]byte, error) {
	if err := accessList.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if !cfg.PreserveResourceID {
		copy := *accessList
		copy.SetResourceID(0)
		copy.SetRevision("")
		accessList = &copy
	}
	return utils.FastMarshal(accessList)
}

// UnmarshalAccessList unmarshals the access list resource from JSON.
func UnmarshalAccessList(data []byte, opts ...MarshalOption) (*accesslist.AccessList, error) {
	if len(data) == 0 {
		return nil, trace.BadParameter("missing access list data")
	}
	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var accessList accesslist.AccessList
	if err := utils.FastUnmarshal(data, &accessList); err != nil {
		return nil, trace.BadParameter(err.Error())
	}
	if err := accessList.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	if cfg.ID != 0 {
		accessList.SetResourceID(cfg.ID)
	}
	if cfg.Revision != "" {
		accessList.SetRevision(cfg.Revision)
	}
	if !cfg.Expires.IsZero() {
		accessList.SetExpiry(cfg.Expires)
	}
	return &accessList, nil
}

// AccessListMembersGetter defines an interface for reading access list members.
type AccessListMembersGetter interface {
	// ListAccessListMembers returns a paginated list of all access list members.
	ListAccessListMembers(ctx context.Context, accessList string, pageSize int, pageToken string) (members []*accesslist.AccessListMember, nextToken string, err error)
	// GetAccessListMember returns the specified access list member resource.
	GetAccessListMember(ctx context.Context, accessList string, memberName string) (*accesslist.AccessListMember, error)
}

// AccessListMembers defines an interface for managing AccessListMembers.
type AccessListMembers interface {
	AccessListMembersGetter

	// UpsertAccessListMember creates or updates an access list member resource.
	UpsertAccessListMember(ctx context.Context, member *accesslist.AccessListMember) (*accesslist.AccessListMember, error)
	// DeleteAccessListMember hard deletes the specified access list member resource.
	DeleteAccessListMember(ctx context.Context, accessList string, memberName string) error
	// DeleteAllAccessListMembersForAccessList hard deletes all access list members for an access list.
	DeleteAllAccessListMembersForAccessList(ctx context.Context, accessList string) error
	// DeleteAllAccessListMembers hard deletes all access list members.
	DeleteAllAccessListMembers(ctx context.Context) error
}

// MarshalAccessListMember marshals the access list member resource to JSON.
func MarshalAccessListMember(member *accesslist.AccessListMember, opts ...MarshalOption) ([]byte, error) {
	if err := member.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if !cfg.PreserveResourceID {
		copy := *member
		copy.SetResourceID(0)
		copy.SetRevision("")
		member = &copy
	}
	return utils.FastMarshal(member)
}

// UnmarshalAccessListMember unmarshals the access list member resource from JSON.
func UnmarshalAccessListMember(data []byte, opts ...MarshalOption) (*accesslist.AccessListMember, error) {
	if len(data) == 0 {
		return nil, trace.BadParameter("missing access list member data")
	}
	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var member accesslist.AccessListMember
	if err := utils.FastUnmarshal(data, &member); err != nil {
		return nil, trace.BadParameter(err.Error())
	}
	if err := member.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	if cfg.ID != 0 {
		member.SetResourceID(cfg.ID)
	}
	if cfg.Revision != "" {
		member.SetRevision(cfg.Revision)
	}
	if !cfg.Expires.IsZero() {
		member.SetExpiry(cfg.Expires)
	}
	return &member, nil
}

// IsAccessListOwner will return true if the user is an owner for the current list.
func IsAccessListOwner(identity tlsca.Identity, accessList *accesslist.AccessList) error {
	isOwner := false
	for _, owner := range accessList.Spec.Owners {
		if owner.Name == identity.Username {
			isOwner = true
			break
		}
	}

	// An opaque access denied error.
	accessDenied := trace.AccessDenied("access denied")

	// User is not an owner, so we'll access denied.
	if !isOwner {
		return accessDenied
	}

	if !UserMeetsRequirements(identity, accessList.Spec.OwnershipRequires) {
		return accessDenied
	}

	// We've gotten through all the checks, so the user is an owner.
	return nil
}

// IsAccessListMember will return true if the user is a member for the current list.
func IsAccessListMember(ctx context.Context, identity tlsca.Identity, clock clockwork.Clock, accessList *accesslist.AccessList, memberGetter AccessListMembersGetter) error {
	username := identity.Username

	member, err := memberGetter.GetAccessListMember(ctx, accessList.GetName(), username)
	if trace.IsNotFound(err) {
		// The member has not been found, so we know they're not a member of this list.
		return trace.NotFound("user %s is not a member of the access list", username)
	} else if err != nil {
		// Some other error has occurred
		return trace.Wrap(err)
	}

	expires := member.Spec.Expires
	if expires.IsZero() {
		return nil
	}

	if !clock.Now().Before(expires) {
		return trace.AccessDenied("user %s's membership has expired in the access list", username)
	}

	if !UserMeetsRequirements(identity, accessList.Spec.MembershipRequires) {
		return trace.AccessDenied("user %s is a member, but does not have the roles or traits required to be a member of this list", username)
	}
	return nil
}

// UserMeetsRequirements will return true if the user meets the requirements for the access list.
func UserMeetsRequirements(identity tlsca.Identity, requires accesslist.Requires) bool {
	// Assemble the user's roles for easy look up.
	userRolesMap := map[string]struct{}{}
	for _, role := range identity.Groups {
		userRolesMap[role] = struct{}{}
	}

	// Check that the user meets the role requirements.
	for _, role := range requires.Roles {
		if _, ok := userRolesMap[role]; !ok {
			return false
		}
	}

	// Assemble traits for easy lookup.
	userTraitsMap := map[string]map[string]struct{}{}
	for k, values := range identity.Traits {
		if _, ok := userTraitsMap[k]; !ok {
			userTraitsMap[k] = map[string]struct{}{}
		}

		for _, v := range values {
			userTraitsMap[k][v] = struct{}{}
		}
	}

	// Check that user meets trait requirements.
	for k, values := range requires.Traits {
		if _, ok := userTraitsMap[k]; !ok {
			return false
		}

		for _, v := range values {
			if _, ok := userTraitsMap[k][v]; !ok {
				return false
			}
		}
	}

	// The user meets all requirements.
	return true
}

// SelectNextReviewDate will select the next review date for the access list.
func SelectNextReviewDate(accessList *accesslist.AccessList) time.Time {
	numMonths := int(accessList.Spec.Audit.Recurrence.Frequency)
	dayOfMonth := int(accessList.Spec.Audit.Recurrence.DayOfMonth)

	// If the last day of the month has been specified, use the 0 day of the
	// next month, which will result in the last day of the target month.
	if dayOfMonth == int(accesslist.LastDayOfMonth) {
		numMonths += 1
		dayOfMonth = 0
	}

	currentReviewDate := accessList.Spec.Audit.NextAuditDate
	nextDate := time.Date(currentReviewDate.Year(), currentReviewDate.Month()+time.Month(numMonths), dayOfMonth,
		0, 0, 0, 0, time.UTC)

	return nextDate
}

// AccessListReviews defines an interface for managing Access List reviews.
type AccessListReviews interface {
	// ListAccessListReviews will list access list reviews for a particular access list.
	ListAccessListReviews(ctx context.Context, accessList string, pageSize int, pageToken string) (reviews []*accesslist.Review, nextToken string, err error)

	// CreateAccessListReview will create a new review for an access list.
	CreateAccessListReview(ctx context.Context, review *accesslist.Review) (updatedReview *accesslist.Review, nextReviewDate time.Time, err error)

	// DeleteAccessListReview will delete an access list review from the backend.
	DeleteAccessListReview(ctx context.Context, accessListName, reviewName string) error

	// DeleteAllAccessListReviews will delete all access list reviews from an access list.
	DeleteAllAccessListReviews(ctx context.Context, accessListName string) error
}

// MarshalAccessListReview marshals the access list review resource to JSON.
func MarshalAccessListReview(review *accesslist.Review, opts ...MarshalOption) ([]byte, error) {
	if err := review.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if !cfg.PreserveResourceID {
		copy := *review
		copy.SetResourceID(0)
		copy.SetRevision("")
		review = &copy
	}
	return utils.FastMarshal(review)
}

// UnmarshalAccessListReview unmarshals the access list review resource from JSON.
func UnmarshalAccessListReview(data []byte, opts ...MarshalOption) (*accesslist.Review, error) {
	if len(data) == 0 {
		return nil, trace.BadParameter("missing access list review data")
	}
	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var review accesslist.Review
	if err := utils.FastUnmarshal(data, &review); err != nil {
		return nil, trace.BadParameter(err.Error())
	}
	if err := review.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	if cfg.ID != 0 {
		review.SetResourceID(cfg.ID)
	}
	if cfg.Revision != "" {
		review.SetRevision(cfg.Revision)
	}
	if !cfg.Expires.IsZero() {
		review.SetExpiry(cfg.Expires)
	}
	return &review, nil
}
