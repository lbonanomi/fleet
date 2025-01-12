package service

import (
	"context"
	"fmt"
	"github.com/fleetdm/fleet/v4/server/authz"
	"github.com/fleetdm/fleet/v4/server/contexts/ctxerr"
	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/service/calendar"
	"github.com/go-kit/log/level"
	"sync"
)

func (svc *Service) CalendarWebhook(ctx context.Context, eventUUID string, channelID string, resourceState string) error {

	appConfig, err := svc.ds.AppConfig(ctx)
	if err != nil {
		return fmt.Errorf("load app config: %w", err)
	}

	if len(appConfig.Integrations.GoogleCalendar) == 0 {
		svc.authz.SkipAuthorization(ctx)
		level.Warn(svc.logger).Log("msg", "Received calendar callback, but Google Calendar integration is not configured")
		return nil
	}
	googleCalendarIntegrationConfig := appConfig.Integrations.GoogleCalendar[0]

	if resourceState == "sync" {
		// This is a sync notification, not a real event
		svc.authz.SkipAuthorization(ctx)
		return nil
	}

	eventDetails, err := svc.ds.GetCalendarEventDetailsByUUID(ctx, eventUUID)
	if err != nil {
		svc.authz.SkipAuthorization(ctx)
		if fleet.IsNotFound(err) {
			// We could try to stop the channel callbacks here, but that may not be secure since we don't know if the request is legitimate
			level.Warn(svc.logger).Log("msg", "Received calendar callback, but did not find corresponding event in database", "event_uuid",
				eventUUID, "channel_id", channelID)
			return err
		}
		return err
	}
	if eventDetails.TeamID == nil {
		// Should not happen
		return fmt.Errorf("calendar event %s has no team ID", eventUUID)
	}

	localConfig := &calendar.CalendarConfig{
		GoogleCalendarIntegration: *googleCalendarIntegrationConfig,
		ServerURL:                 appConfig.ServerSettings.ServerURL,
	}
	userCalendar := calendar.CreateUserCalendarFromConfig(ctx, localConfig, svc.logger)

	// Authenticate request. We will use the channel ID for authentication.
	svc.authz.SkipAuthorization(ctx)
	savedChannelID, err := userCalendar.Get(&eventDetails.CalendarEvent, "channelID")
	if err != nil {
		return ctxerr.Wrap(ctx, err, "get channel ID")
	}
	if savedChannelID != channelID {
		return authz.ForbiddenWithInternal(fmt.Sprintf("calendar channel ID mismatch: %s != %s", savedChannelID, channelID), nil, nil, nil)
	}

	genBodyFn := func(conflict bool) (body string, ok bool, err error) {

		// This function is called when a new event is being created.
		var team *fleet.Team
		team, err = svc.ds.TeamWithoutExtras(ctx, *eventDetails.TeamID)
		if err != nil {
			return "", false, err
		}

		if team.Config.Integrations.GoogleCalendar == nil ||
			!team.Config.Integrations.GoogleCalendar.Enable {
			return "", false, nil
		}

		var policies []fleet.PolicyCalendarData
		policies, err = svc.ds.GetCalendarPolicies(ctx, team.ID)
		if err != nil {
			return "", false, err
		}

		if len(policies) == 0 {
			return "", false, nil
		}

		policyIDs := make([]uint, 0, len(policies))
		for _, policy := range policies {
			policyIDs = append(policyIDs, policy.ID)
		}

		var hosts []fleet.HostPolicyMembershipData
		hosts, err = svc.ds.GetTeamHostsPolicyMemberships(ctx, googleCalendarIntegrationConfig.Domain, team.ID, policyIDs,
			&eventDetails.HostID)
		if err != nil {
			return "", false, err
		}
		if len(hosts) != 1 {
			return "", false, nil
		}
		host := hosts[0]
		if host.Passing { // host is passing all configured policies
			return "", false, nil
		}
		if host.Email == "" {
			err = fmt.Errorf("host %d has no associated email", host.HostID)
			return "", false, err
		}

		return calendar.GenerateCalendarEventBody(ctx, svc.ds, team.Name, host, &sync.Map{}, conflict, svc.logger), true, nil
	}

	err = userCalendar.Configure(eventDetails.Email)
	if err != nil {
		return ctxerr.Wrap(ctx, err, "configure calendar")
	}
	event, updated, err := userCalendar.GetAndUpdateEvent(&eventDetails.CalendarEvent, genBodyFn)
	if err != nil {
		return ctxerr.Wrap(ctx, err, "get and update event")
	}
	if updated && event != nil {
		// Event was updated, so we need to save it
		_, err = svc.ds.CreateOrUpdateCalendarEvent(ctx, event.UUID, event.Email, event.StartTime, event.EndTime, event.Data,
			event.TimeZone, eventDetails.ID, fleet.CalendarWebhookStatusNone)
		if err != nil {
			return ctxerr.Wrap(ctx, err, "create or update calendar event")
		}
	}
	return nil
}
