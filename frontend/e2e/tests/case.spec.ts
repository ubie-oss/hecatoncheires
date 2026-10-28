import { CaseFieldFilters } from '../pages/CaseFieldFilters';
import { test, expect } from '@playwright/test';
import { CaseListPage } from '../pages/CaseListPage';
import { CaseFormPage } from '../pages/CaseFormPage';
import { CaseDetailPage } from '../pages/CaseDetailPage';
import { ActionFormPage } from '../pages/ActionFormPage';
import { ActionDetailPage } from '../pages/ActionDetailPage';
import { TEST_WORKSPACE_ID } from '../fixtures/testData';

test.describe('Case Management', () => {
  test.beforeEach(async ({ page }) => {
    // Navigate to case list page
    const caseListPage = new CaseListPage(page);
    await caseListPage.navigate(TEST_WORKSPACE_ID);
    await caseListPage.waitForTableLoad();
  });

  test('should display case list page', async ({ page }) => {
    const caseListPage = new CaseListPage(page);

    // Verify the page is loaded
    const isLoaded = await caseListPage.isPageLoaded();
    expect(isLoaded).toBeTruthy();
  });

  test('should create a new case', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);

    // Click "New Case" button
    await caseListPage.clickNewCaseButton();

    // Fill in the form
    await caseFormPage.createCase({
      title: 'E2E Test Case',
      description: 'This is a test case created by Playwright',
      customFields: {
        category: 'bug',
      },
    });

    // Verify the case appears in the list
    await caseListPage.fillSearchFilter('E2E Test Case');
    const exists = await caseListPage.caseExists('E2E Test Case');
    expect(exists).toBeTruthy();
  });

  // config.test.toml sets [slack] allow_existing_channel = true, but the E2E
  // backend runs without a Slack bot token, so the server reports the feature as
  // unavailable and the create form must keep the plain new-channel flow.
  // Linking an existing channel itself needs the Slack API (conversations.info /
  // members / join), so it is covered by the usecase and Vitest tests instead.
  test('should keep the existing-channel choice hidden without Slack and still create a case', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);

    await caseListPage.clickNewCaseButton();
    await caseFormPage.waitForFormVisible();
    await expect(page.getByTestId('private-case-checkbox')).toBeVisible();
    expect(await caseFormPage.isChannelSourceVisible()).toBeFalsy();

    await caseFormPage.createCase({
      title: 'E2E No Existing Channel Choice',
      customFields: { category: 'bug' },
    });

    await caseListPage.fillSearchFilter('E2E No Existing Channel Choice');
    expect(await caseListPage.caseExists('E2E No Existing Channel Choice')).toBeTruthy();
  });

  test('should view case details', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create a test case first
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case for Detail View',
      description: 'Testing case detail view',
      customFields: {
        category: 'bug',
      },
    });

    // Wait for table to update
    await caseListPage.waitForTableLoad();

    // Click on the case to view details
    await caseListPage.fillSearchFilter('Case for Detail View');
    await caseListPage.clickCaseByTitle('Case for Detail View');

    // Verify detail page is loaded
    const isLoaded = await caseDetailPage.isPageLoaded();
    expect(isLoaded).toBeTruthy();

    // Verify the title matches
    const title = await caseDetailPage.getTitle();
    expect(title).toContain('Case for Detail View');
  });

  test('should edit a case', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create a test case
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case to Edit',
      description: 'Original description',
      customFields: {
        category: 'feature',
      },
    });

    // Wait for table to update
    await caseListPage.waitForTableLoad();

    // Open the case detail
    await caseListPage.fillSearchFilter('Case to Edit');
    await caseListPage.clickCaseByTitle('Case to Edit');

    // Update the description via Linear-style inline editing
    await caseDetailPage.setDescription('Updated description');

    // Verify the description was updated
    const description = await caseDetailPage.getDescription();
    expect(description).toContain('Updated description');
  });

  test('should mark a case as a test on create, show the badge, and toggle it on detail', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    const title = `Test Flag Case ${Date.now()}`;

    // Create the case with the "Test case" checkbox ticked.
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title,
      description: 'Filed as a test',
      customFields: { category: 'bug' },
      isTest: true,
    });

    // The list row shows the Test badge.
    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter(title);
    expect(await caseListPage.caseExists(title)).toBeTruthy();
    expect(await caseListPage.caseRowHasTestBadge(title)).toBeTruthy();

    // Detail page shows the badge and the toggle is checked.
    await caseListPage.clickCaseByTitle(title);
    await caseDetailPage.waitForPageLoad();
    expect(await caseDetailPage.hasTestBadge()).toBeTruthy();
    expect(await caseDetailPage.isTestToggleChecked()).toBeTruthy();

    // Toggle it off; the badge disappears and the change round-trips to the
    // backend (survives a reload).
    await caseDetailPage.setTestToggle(false);
    await page.reload();
    await caseDetailPage.waitForPageLoad();
    expect(await caseDetailPage.hasTestBadge()).toBeFalsy();
    expect(await caseDetailPage.isTestToggleChecked()).toBeFalsy();
  });

  test('should navigate from case detail to the filtered action list', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create a case to land on its detail page.
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case With Action Link',
      description: 'Cross-link to action list',
      customFields: {
        category: 'bug',
      },
    });

    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter('Case With Action Link');
    await caseListPage.clickCaseByTitle('Case With Action Link');
    await caseDetailPage.waitForPageLoad();

    // The "Open in action list" link must be visible on the OPEN case.
    const linkVisible = await caseDetailPage.isOpenInActionsLinkVisible();
    expect(linkVisible).toBeTruthy();

    // Capture the numeric case id straight from the URL so the assertion
    // doesn't rely on the synthetic order the API hands back.
    const detailMatch = page.url().match(/\/cases\/(\d+)/);
    expect(detailMatch).not.toBeNull();
    const caseId = detailMatch![1];

    await caseDetailPage.clickOpenInActions();
    expect(page.url()).toContain(`/actions/case/${caseId}`);

    // The action-list page should mark this case as the active filter on
    // the new searchable dropdown.
    const trigger = page.getByTestId('action-case-filter-trigger');
    await trigger.waitFor({ state: 'visible', timeout: 5000 });
    await expect(trigger).toContainText(`#${caseId}`);

    // The selected value should render as a visually distinct chip
    // (vs. the muted "All Case" state), and the full "#id title" string
    // should be exposed via the trigger's `title` attribute so an
    // ellipsised long title stays inspectable.
    const chip = page.getByTestId('action-case-filter-chip');
    await expect(chip).toBeVisible();
    await expect(chip).toContainText(`#${caseId}`);
    await expect(chip).toContainText('Case With Action Link');
    await expect(trigger).toHaveAttribute(
      'title',
      `#${caseId} Case With Action Link`,
    );
  });

  test('should not display Slack channel button when slackChannelID is empty', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create a case (without Slack configured, slackChannelID will be empty)
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case Without Slack',
      description: 'No Slack channel linked',
      customFields: {
        category: 'bug',
      },
    });

    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter('Case Without Slack');
    await caseListPage.clickCaseByTitle('Case Without Slack');
    await caseDetailPage.waitForPageLoad();

    // Slack channel button should not be visible
    const isVisible = await caseDetailPage.isSlackChannelButtonVisible();
    expect(isVisible).toBeFalsy();
  });

  test('should display Slack channel button when slackChannelID is present', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create a case first
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case With Slack',
      description: 'Has Slack channel linked',
      customFields: {
        category: 'bug',
      },
    });

    await caseListPage.waitForTableLoad();

    // Intercept GraphQL response to inject Slack channel data
    await page.route('**/graphql', async (route) => {
      const request = route.request();
      const postData = request.postDataJSON();

      // Only intercept GetCase query
      if (postData?.operationName === 'GetCase') {
        const response = await route.fetch();
        const json = await response.json();

        // Inject Slack channel data into the response. slackChannelName
        // is intentionally not injected: GetCase no longer requests it,
        // and the sidebar link now uses a generic "Open in Slack" label
        // driven by slackChannelID alone.
        if (json.data?.case) {
          json.data.case.slackChannelID = 'C1234567890';
          json.data.case.slackChannelURL = 'https://test-workspace.slack.com/archives/C1234567890';
        }

        await route.fulfill({ response, json });
      } else {
        await route.continue();
      }
    });

    await caseListPage.fillSearchFilter('Case With Slack');
    await caseListPage.clickCaseByTitle('Case With Slack');
    await caseDetailPage.waitForPageLoad();

    // Slack channel button should be visible
    const isVisible = await caseDetailPage.isSlackChannelButtonVisible();
    expect(isVisible).toBeTruthy();

    // Verify button href points to Slack. The label is now a generic
    // "Open in Slack" i18n string driven by slackChannelID alone, so we
    // no longer assert on the visible text.
    const href = await caseDetailPage.getSlackChannelButtonHref();
    expect(href).toBe('https://test-workspace.slack.com/archives/C1234567890');
  });

  test('should list multiple cases', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);

    // Use a unique prefix so this test stays robust when the backend
    // already holds cases from other tests / workers — the list view is
    // paginated and a generic "Case N" title could fall off page 1 once
    // enough unrelated rows exist (root cause of an earlier flaky run).
    const prefix = `Listed-${Date.now()}`;
    const caseCount = 3;
    for (let i = 1; i <= caseCount; i++) {
      await caseListPage.clickNewCaseButton();
      await caseFormPage.createCase({
        title: `${prefix} ${i}`,
        description: `Description for case ${i}`,
        customFields: {
          category: 'task',
        },
      });
    }

    // Reload to get fresh data
    await page.reload();
    await caseListPage.waitForTableLoad();

    // Verify all cases are listed. Filter by exact title every time so
    // pagination cannot hide the row.
    for (let i = 1; i <= caseCount; i++) {
      const title = `${prefix} ${i}`;
      await caseListPage.fillSearchFilter(title);
      const exists = await caseListPage.caseExists(title);
      expect(exists).toBeTruthy();
    }
  });

  test('should create a case without description', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Click "New Case" button
    await caseListPage.clickNewCaseButton();

    // Create case with title only (no description)
    await caseFormPage.createCase({
      title: 'Case Without Description',
      customFields: {
        category: 'bug',
      },
    });

    // Verify the case appears in the list
    await caseListPage.fillSearchFilter('Case Without Description');
    const exists = await caseListPage.caseExists('Case Without Description');
    expect(exists).toBeTruthy();

    // Verify case detail
    await caseListPage.clickCaseByTitle('Case Without Description');
    const title = await caseDetailPage.getTitle();
    expect(title).toContain('Case Without Description');
  });

  test('should filter cases by search text', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);

    // Create cases with different titles
    const cases = ['Alpha Issue', 'Beta Bug', 'Gamma Issue'];
    for (const title of cases) {
      await caseListPage.clickNewCaseButton();
      await caseFormPage.createCase({
        title,
        description: `Description for ${title}`,
        customFields: { category: 'task' },
      });
    }

    await page.reload();
    await caseListPage.waitForTableLoad();

    // Search for "Issue"
    await caseListPage.fillSearchFilter('Issue');

    // Verify filtered results
    const alphaExists = await caseListPage.caseExists('Alpha Issue');
    expect(alphaExists).toBeTruthy();
    const gammaExists = await caseListPage.caseExists('Gamma Issue');
    expect(gammaExists).toBeTruthy();
    const betaExists = await caseListPage.caseExists('Beta Bug');
    expect(betaExists).toBeFalsy();

    // Clear filter and verify all cases appear by searching each one
    for (const title of cases) {
      await caseListPage.fillSearchFilter(title);
      const exists = await caseListPage.caseExists(title);
      expect(exists).toBeTruthy();
    }
  });

  test('should toggle column visibility', async ({ page }) => {
    const caseListPage = new CaseListPage(page);

    // Status is a toggleable built-in column shown by default
    let isVisible = await caseListPage.isColumnVisible('Status');
    expect(isVisible).toBeTruthy();

    // Open column selector and toggle Status off
    await caseListPage.openColumnSelector();
    await caseListPage.toggleColumn('status');

    isVisible = await caseListPage.isColumnVisible('Status');
    expect(isVisible).toBeFalsy();

    // Toggle Status back on
    await caseListPage.toggleColumn('status');
    isVisible = await caseListPage.isColumnVisible('Status');
    expect(isVisible).toBeTruthy();
  });

  test('should treat a case row as a link: modifier-click opens a new tab, any cell navigates', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const title = `RowLink-${Date.now()}`;

    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({ title, customFields: { category: 'task' } });
    await caseListPage.fillSearchFilter(title);

    // The row navigates through a real anchor, so the browser's own
    // Cmd/Ctrl-click behaviour applies and opens the detail page in a new tab.
    const href = await caseListPage.getCaseHrefByTitle(title);
    expect(href).toMatch(new RegExp(`^/ws/${TEST_WORKSPACE_ID}/cases/\\d+$`));

    const newTab = await caseListPage.openCaseInNewTabByTitle(title);
    expect(new URL(newTab.url()).pathname).toBe(href);
    // The original tab stays on the list.
    expect(new URL(page.url()).pathname).toBe(`/ws/${TEST_WORKSPACE_ID}/cases`);
    await newTab.close();

    // Clicking outside the title cell reaches the same link.
    await caseListPage.clickCaseRowCellByHeader(title, 'Created');
    expect(new URL(page.url()).pathname).toBe(href);

    // A case created in the Web UI has no Slack channel, so its Slack cell only
    // shows a dash — it must still belong to the row link.
    await page.goBack();
    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter(title);
    await caseListPage.clickCaseRowCellByHeader(title, 'Slack');
    expect(new URL(page.url()).pathname).toBe(href);
  });

  test('should keep the originating tab after opening and closing a related action', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);
    const actionFormPage = new ActionFormPage(page);
    const actionDetailPage = new ActionDetailPage(page);
    const title = `ReturnTo-${Date.now()}`;
    const actionTitle = `${title} Action`;

    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({ title, customFields: { category: 'task' } });
    await caseListPage.fillSearchFilter(title);
    await caseListPage.clickCaseByTitle(title);
    await caseDetailPage.waitForPageLoad();

    await page.getByTestId('add-action-button').click();
    await actionFormPage.createAction({ title: actionTitle, caseTitle: title });

    // Re-enter from the All tab so the return target is not the default one.
    await caseListPage.navigate(TEST_WORKSPACE_ID);
    await caseListPage.clickStatusTab('All');
    await caseListPage.fillSearchFilter(title);
    await caseListPage.clickCaseByTitle(title);
    await caseDetailPage.waitForPageLoad();

    // The Action modal is a route change on the same Case. Opening and closing
    // it must not lose which list tab (or page) the Case was opened from.
    await page.locator('a').filter({ hasText: actionTitle }).first().click();
    await expect(page).toHaveURL(/\/actions\/\d+$/);
    await actionDetailPage.closeModal();
    await expect(page).toHaveURL(/\/cases\/\d+$/);

    await caseDetailPage.clickBack();
    await caseListPage.waitForTableLoad();
    expect(page.url()).toContain('status=all');
  });

  test('should paginate cases, adjust the page size, and return to the same page', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Unique prefix so the exact-count assertions below survive a retry or a
    // parallel worker: a generic "Paginated Case" filter would match a
    // previous attempt's 21 rows too and break the "1 / 2" / 20-row math.
    const prefix = `Paginated-${Date.now()}`;

    // Create 21 cases to trigger pagination
    for (let i = 1; i <= 21; i++) {
      await caseListPage.clickNewCaseButton();
      await caseFormPage.createCase({
        title: `${prefix} ${i}`,
        customFields: { category: 'task' },
      });
    }

    await page.reload();
    await caseListPage.waitForTableLoad();

    // Filter to only show cases from this test to avoid interference
    await caseListPage.fillSearchFilter(prefix);

    // Verify first page has 20 rows
    const rowCount = await caseListPage.getRowCount();
    expect(rowCount).toBe(20);

    // Verify pagination info
    const paginationInfo = await caseListPage.getPaginationInfo();
    expect(paginationInfo).toContain('1 / 2');

    // Go to next page
    await caseListPage.goToPage('next');
    const page2RowCount = await caseListPage.getRowCount();
    expect(page2RowCount).toBeGreaterThanOrEqual(1);

    // Go back to first page
    await caseListPage.goToPage('prev');
    const backInfo = await caseListPage.getPaginationInfo();
    expect(backInfo).toContain('1 / 2');

    // Raising the page size collapses the same 21 rows onto one page.
    await caseListPage.setPageSize(50);
    expect(await caseListPage.getRowCount()).toBe(21);
    expect(await caseListPage.getPaginationInfo()).toContain('1 / 1');

    // The choice is persisted, so a reload comes back at 50 rows. The search
    // filter is not persisted, so re-apply it before counting again.
    await page.reload();
    await caseListPage.waitForTableLoad();
    expect(await caseListPage.getPageSize()).toBe(50);
    await caseListPage.setPageSize(20);
    await caseListPage.fillSearchFilter(prefix);
    expect(await caseListPage.getPaginationInfo()).toContain('1 / 2');

    // Opening a case from page 2 and coming back must land on page 2 again.
    // The search filter is component state and is gone after the round trip,
    // so the total page count differs — only the page index is asserted.
    await caseListPage.goToPage('next');
    expect(page.url()).toContain('page=2');
    await caseListPage.clickFirstCase();

    await caseDetailPage.clickBack();
    await caseListPage.waitForTableLoad();
    expect(page.url()).toContain('page=2');
    expect(await caseListPage.getPaginationInfo()).toMatch(/^\s*2 \//);

    // Browser Back gets the same treatment, because the page lives in the URL.
    await caseListPage.clickFirstCase();
    await page.goBack();
    await caseListPage.waitForTableLoad();
    expect(page.url()).toContain('page=2');
  });

  test('should reflect closed case in list after closing', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create a case
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case To Close',
      description: 'Will be closed',
      customFields: { category: 'task' },
    });
    await caseListPage.waitForTableLoad();

    // Verify it's in Open tab
    await caseListPage.fillSearchFilter('Case To Close');
    const existsInOpen = await caseListPage.caseExists('Case To Close');
    expect(existsInOpen).toBeTruthy();

    // Open detail and close the case
    await caseListPage.clickCaseByTitle('Case To Close');
    await caseDetailPage.waitForPageLoad();
    await caseDetailPage.clickCloseButton();

    // Go back to list
    await caseDetailPage.clickBack();
    await caseListPage.waitForTableLoad();

    // Verify it's no longer in Open tab
    const existsAfterClose = await caseListPage.caseExists('Case To Close');
    expect(existsAfterClose).toBeFalsy();

    // Switch to Closed tab and verify it's there
    await caseListPage.clickStatusTab('Closed');
    const existsInClosed = await caseListPage.caseExists('Case To Close');
    expect(existsInClosed).toBeTruthy();
  });

  test('should display timestamps on case detail page', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create a case
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case With Timestamps',
      description: 'Testing timestamps display',
      customFields: { category: 'bug' },
    });

    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter('Case With Timestamps');
    await caseListPage.clickCaseByTitle('Case With Timestamps');
    await caseDetailPage.waitForPageLoad();

    // Verify timestamps are visible
    const timestampsVisible = await caseDetailPage.isTimestampsVisible();
    expect(timestampsVisible).toBeTruthy();

    // The case was just created, so both timestamps must render a real value,
    // not the "—" placeholder shown for a missing/invalid one.
    expect(await caseDetailPage.getCreatedTimestamp()).not.toBe('—');
    expect(await caseDetailPage.getUpdatedTimestamp()).not.toBe('—');

    // For the recency check, parse the machine-readable ISO 8601 value from the
    // `data-ts` attribute rather than the localized display text — Date.parse
    // on a localized string is locale-dependent and unreliable. Both stamps
    // must fall within the last 10 minutes.
    const RECENT_MS = 10 * 60 * 1000;

    const createdMs = Date.parse(await caseDetailPage.getCreatedTimestampISO());
    expect(Number.isNaN(createdMs)).toBeFalsy();
    expect(Math.abs(Date.now() - createdMs)).toBeLessThan(RECENT_MS);

    const updatedMs = Date.parse(await caseDetailPage.getUpdatedTimestampISO());
    expect(Number.isNaN(updatedMs)).toBeFalsy();
    expect(Math.abs(Date.now() - updatedMs)).toBeLessThan(RECENT_MS);
  });

  test('should display Fields section on case detail page', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create a case with custom fields
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case With Fields',
      description: 'Testing fields display',
      customFields: { category: 'bug' },
    });

    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter('Case With Fields');
    await caseListPage.clickCaseByTitle('Case With Fields');
    await caseDetailPage.waitForPageLoad();

    // Verify Fields section is visible
    const fieldsVisible = await caseDetailPage.isFieldsSectionVisible();
    expect(fieldsVisible).toBeTruthy();
  });

  test('should display empty action state for new case', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create a case (no actions)
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case With No Actions',
      description: 'Testing empty action state',
      customFields: { category: 'task' },
    });

    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter('Case With No Actions');
    await caseListPage.clickCaseByTitle('Case With No Actions');
    await caseDetailPage.waitForPageLoad();

    // Verify empty action state is visible
    const emptyStateVisible = await caseDetailPage.isEmptyActionStateVisible();
    expect(emptyStateVisible).toBeTruthy();
  });

  test('should show status badge on case detail page', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create a case
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case Status Badge',
      description: 'Testing status badge',
      customFields: { category: 'task' },
    });

    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter('Case Status Badge');
    await caseListPage.clickCaseByTitle('Case Status Badge');
    await caseDetailPage.waitForPageLoad();

    // Verify status badge shows "Open"
    const status = await caseDetailPage.getStatus();
    expect(status).toBe('Open');
  });

  test('should reflect reopened case in list after reopening', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    // Create and close a case
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title: 'Case To Reopen',
      description: 'Will be reopened',
      customFields: { category: 'task' },
    });
    await caseListPage.waitForTableLoad();

    // Filter to find the case reliably in case of pagination
    await caseListPage.fillSearchFilter('Case To Reopen');
    await caseListPage.clickCaseByTitle('Case To Reopen');
    await caseDetailPage.waitForPageLoad();
    await caseDetailPage.clickCloseButton();
    await caseDetailPage.clickBack();
    await caseListPage.waitForTableLoad();

    // Switch to Closed tab
    await caseListPage.clearSearchFilter();
    await caseListPage.clickStatusTab('Closed');
    const existsInClosed = await caseListPage.caseExists('Case To Reopen');
    expect(existsInClosed).toBeTruthy();

    // Reopen the case
    await caseListPage.clickCaseByTitle('Case To Reopen');
    await caseDetailPage.waitForPageLoad();
    await caseDetailPage.clickReopenButton();

    // Go back to list
    await caseDetailPage.clickBack();
    await caseListPage.waitForTableLoad();

    // Verify it's no longer in Closed tab
    await caseListPage.clickStatusTab('Closed');
    const existsAfterReopen = await caseListPage.caseExists('Case To Reopen');
    expect(existsAfterReopen).toBeFalsy();

    // Switch to Open tab and verify it's there
    await caseListPage.clickStatusTab('Open');
    await caseListPage.fillSearchFilter('Case To Reopen');
    const existsInOpen = await caseListPage.caseExists('Case To Reopen');
    expect(existsInOpen).toBeTruthy();
  });

  test('should show and edit a markdown field in the main column below the description', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);

    const title = `Markdown Field Case ${Date.now()}`;
    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({ title, customFields: { category: 'task' } });
    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter(title);
    await caseListPage.clickCaseByTitle(title);
    await caseDetailPage.waitForPageLoad();

    // The markdown 'runbook' field has its own section in the main column,
    // not a row in the sidebar Fields list.
    const section = page.getByTestId('case-field-runbook');
    await expect(section).toBeVisible();
    await expect(section).toContainText('Runbook');
    await expect(page.locator('aside').getByTestId('case-field-runbook')).toHaveCount(0);
    await expect(page.getByTestId('case-fields-inline')).not.toContainText('Runbook');

    // Click the (empty) value to edit in place, type markdown.
    await page.getByTestId('field-runbook').click();
    const source = '## Steps\n\n1. first\n2. second';
    await page.getByTestId('field-runbook-input').fill(source);

    // The editor's Preview tab renders it.
    await page.getByTestId('field-runbook-tab-preview').click();
    await expect(page.getByTestId('field-runbook-preview').locator('h2')).toHaveText('Steps');

    // Save; the section renders the saved markdown inline.
    await page.getByTestId('field-runbook-save').click();
    await expect(section.getByRole('heading', { level: 2, name: 'Steps' })).toBeVisible();

    // Reload to confirm the value round-tripped to the backend.
    await page.reload();
    await caseDetailPage.waitForPageLoad();
    await expect(page.getByTestId('case-field-runbook').getByRole('heading', { level: 2, name: 'Steps' })).toBeVisible();
  });

  // Archiving takes a CLOSED case out of the Closed tab and the board without
  // deleting it. The round trip is what matters: the case has to come back.
  test('should archive a closed case in bulk and restore it from the Archived tab', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);
    const title = `Case To Archive ${Date.now()}`;

    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title,
      description: 'Will be archived',
      customFields: { category: 'task' },
    });
    await caseListPage.waitForTableLoad();

    await caseListPage.fillSearchFilter(title);
    await caseListPage.clickCaseByTitle(title);
    await caseDetailPage.waitForPageLoad();
    await caseDetailPage.clickCloseButton();
    await caseDetailPage.clickBack();
    await caseListPage.waitForTableLoad();

    // Closed tab: the row is there and selectable, and the bar offers Archive
    // (not the Drafts tab's Submit / Delete).
    await caseListPage.clickStatusTab('Closed');
    await caseListPage.fillSearchFilter(title);
    expect(await caseListPage.caseExists(title)).toBeTruthy();

    await caseListPage.selectRowByTitle(title);
    expect(await caseListPage.getBulkSelectedCount()).toBe(1);
    expect(await caseListPage.hasBulkAction('archive')).toBeTruthy();
    expect(await caseListPage.hasBulkAction('delete')).toBeFalsy();

    await caseListPage.clickBulkArchive();

    // The row leaves the Closed tab as soon as the call is accepted.
    await expect(caseListPage.getCaseRowByTitle(title)).toHaveCount(0);

    // Bulk archiving is asynchronous server-side: the mutation returns the
    // accepted ids and the writes happen after the response. Reload so the
    // assertion below reads persisted state rather than racing that tail.
    await page.reload();
    await caseListPage.waitForTableLoad();

    // Archived tab: the case is there, and only Restore is offered.
    await caseListPage.clickStatusTab('Archived');
    await caseListPage.fillSearchFilter(title);
    await expect(caseListPage.getCaseRowByTitle(title)).toHaveCount(1);

    await caseListPage.selectRowByTitle(title);
    expect(await caseListPage.hasBulkAction('unarchive')).toBeTruthy();
    expect(await caseListPage.hasBulkAction('archive')).toBeFalsy();

    await caseListPage.clickBulkUnarchive();
    await expect(caseListPage.getCaseRowByTitle(title)).toHaveCount(0);

    // Back on the Closed tab after a reload, i.e. the restore reached the
    // backend rather than only the local view.
    await page.reload();
    await caseListPage.waitForTableLoad();
    await caseListPage.clickStatusTab('Closed');
    await caseListPage.fillSearchFilter(title);
    await expect(caseListPage.getCaseRowByTitle(title)).toHaveCount(1);

    // …and gone from the Archived tab.
    await caseListPage.clickStatusTab('Archived');
    await caseListPage.fillSearchFilter(title);
    await expect(caseListPage.getCaseRowByTitle(title)).toHaveCount(0);
  });

  test('should archive and restore a single case from the detail page', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);
    const title = `Case Detail Archive ${Date.now()}`;

    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title,
      description: 'Archived from the detail page',
      customFields: { category: 'task' },
    });
    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter(title);
    await caseListPage.clickCaseByTitle(title);
    await caseDetailPage.waitForPageLoad();

    // An OPEN case offers no archive item: only a CLOSED one can be archived.
    expect(await caseDetailPage.kebabOffers('archive')).toBeFalsy();
    await page.keyboard.press('Escape');
    await page.mouse.click(5, 5);

    await caseDetailPage.clickCloseButton();
    await caseDetailPage.clickArchive();
    await expect(caseDetailPage.archivedBadge()).toBeVisible();

    // The detail page stays reachable by URL while the case is archived, and
    // the badge survives a reload — i.e. the change was persisted.
    const archivedUrl = page.url();
    await page.reload();
    await caseDetailPage.waitForPageLoad();
    expect(page.url()).toBe(archivedUrl);
    await expect(caseDetailPage.archivedBadge()).toBeVisible();

    await caseDetailPage.clickUnarchive();
    await expect(caseDetailPage.archivedBadge()).toHaveCount(0);
  });

  test('should keep an archived case off the case board', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    const caseDetailPage = new CaseDetailPage(page);
    const title = `Case Board Archive ${Date.now()}`;

    await caseListPage.clickNewCaseButton();
    await caseFormPage.createCase({
      title,
      description: 'Should leave the board once archived',
      customFields: { category: 'task' },
    });
    await caseListPage.waitForTableLoad();
    await caseListPage.fillSearchFilter(title);
    await caseListPage.clickCaseByTitle(title);
    await caseDetailPage.waitForPageLoad();
    await caseDetailPage.clickCloseButton();
    await caseDetailPage.clickArchive();
    await expect(caseDetailPage.archivedBadge()).toBeVisible();

    // The Cases list's own default (Open) never showed it once closed; the
    // point here is that no listing driven by the default archive scope does,
    // and the All tab is the widest of those.
    await caseListPage.navigate(TEST_WORKSPACE_ID);
    await caseListPage.waitForTableLoad();
    await caseListPage.clickStatusTab('All');
    await caseListPage.fillSearchFilter(title);
    await expect(caseListPage.getCaseRowByTitle(title)).toHaveCount(0);

    await caseListPage.clickStatusTab('Closed');
    await caseListPage.fillSearchFilter(title);
    await expect(caseListPage.getCaseRowByTitle(title)).toHaveCount(0);

    // Only the Archived tab, which asks for the archived slice, shows it.
    await caseListPage.clickStatusTab('Archived');
    await caseListPage.fillSearchFilter(title);
    await expect(caseListPage.getCaseRowByTitle(title)).toHaveCount(1);
  });
});

test('shares category filters, combines fields and restores them from case detail', async ({ page }) => {
  const list = new CaseListPage(page);
  const form = new CaseFormPage(page);
  const filters = new CaseFieldFilters(page);
  const prefix = `Field filter ${Date.now()}`;
  await list.navigate('test');
  for (const [category, priority] of [['bug', 'high'], ['bug', 'low'], ['feature', 'high'], ['feature', 'low'], ['task', 'medium']]) {
    await list.clickNewCaseButton();
    await form.createCase({ title: `${prefix} ${category} ${priority}`, customFields: { category, priority, description: `${prefix} ${priority}` } });
  }
  await filters.toggleOption('category', 'Bug');
  await filters.toggleOption('priority', 'High');
  await expect(page).toHaveURL(/field.category=bug.*field.priority=high/);
  await expect(list.getCaseRowByTitle(`${prefix} bug high`)).toBeVisible();
  await expect(list.getCaseRowByTitle(`${prefix} bug low`)).toHaveCount(0);
  await expect(list.getCaseRowByTitle(`${prefix} feature high`)).toHaveCount(0);
  const sharedUrl = page.url();
  await page.reload();
  await expect(list.getCaseRowByTitle(`${prefix} bug high`)).toBeVisible();
  await expect(list.getCaseRowByTitle(`${prefix} feature high`)).toHaveCount(0);
  await list.clickCaseByTitle(`${prefix} bug high`);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(page).toHaveURL(sharedUrl);
  await expect(list.getCaseRowByTitle(`${prefix} bug low`)).toHaveCount(0);
  await filters.toggleOption('category', 'Feature');
  await expect(list.getCaseRowByTitle(`${prefix} feature high`)).toBeVisible();
  await filters.toggleOption('priority', 'Low');
  await expect(list.getCaseRowByTitle(`${prefix} bug low`)).toBeVisible();
  await expect(list.getCaseRowByTitle(`${prefix} feature low`)).toBeVisible();
  await expect(list.getCaseRowByTitle(`${prefix} task medium`)).toHaveCount(0);
  await filters.addValue('description', 'Description', 'no matching value');
  await expect(list.getCaseRowByTitle(`${prefix} bug high`)).toHaveCount(0);
  await filters.addValue('description', 'Description', `${prefix} high`);
  await expect(list.getCaseRowByTitle(`${prefix} bug high`)).toBeVisible();
  await expect(list.getCaseRowByTitle(`${prefix} bug low`)).toHaveCount(0);
  await filters.addValue('description', 'Description', `${prefix} low`);
  await expect(list.getCaseRowByTitle(`${prefix} bug low`)).toBeVisible();
  await filters.close();
  const summary = page.getByTestId('case-field-filters-summary');
  await summary.getByRole('button', { name: 'Remove no matching value from Description', exact: true }).click();
  const compoundUrl = page.url();
  await page.reload();
  await expect(page).toHaveURL(compoundUrl);
  await expect(summary).toContainText('Bug');
  await expect(summary).toContainText('Feature');
  await expect(summary).toContainText('High');
  await expect(summary).toContainText('Low');
  await expect(list.getCaseRowByTitle(`${prefix} feature low`)).toBeVisible();
  await summary.getByRole('button', { name: 'Remove Low from Priority', exact: true }).click();
  await expect(list.getCaseRowByTitle(`${prefix} bug low`)).toHaveCount(0);
  await expect(page).not.toHaveURL(/field.priority=low/);
  await summary.getByRole('button', { name: 'Remove Priority condition', exact: true }).click();
  await expect(list.getCaseRowByTitle(`${prefix} bug low`)).toBeVisible();
  await filters.clear();
  await list.fillSearchFilter(prefix);
  await expect(list.getCaseRowByTitle(`${prefix} bug low`)).toBeVisible();
  await expect(page).not.toHaveURL(/field\./);
});
