import { CaseFieldFilters } from '../pages/CaseFieldFilters';
import { test, expect } from '@playwright/test';
import { CaseKanbanPage } from '../pages/CaseKanbanPage';
import { ActionListPage } from '../pages/ActionListPage';
import { CaseListPage } from '../pages/CaseListPage';
import { CaseFormPage } from '../pages/CaseFormPage';
import { CaseDetailPage } from '../pages/CaseDetailPage';
import { ActionFormPage } from '../pages/ActionFormPage';
import { ActionDetailPage } from '../pages/ActionDetailPage';

// The "review" workspace (frontend/e2e/fixtures/config.review.test.toml) is
// configured in thread mode with a [case.status] set, so its board renders
// Cases instead of Actions.
const THREAD_WS = 'review';
const CHANNEL_WS = 'test';

test.describe('Thread-mode Case board', () => {
  test('renders the Case Kanban for a thread-mode workspace', async ({ page }) => {
    const board = new CaseKanbanPage(page);
    await board.navigate(THREAD_WS);
    await board.waitForBoardLoad();

    // The thread-mode Case board is shown (not the Action board).
    expect(await board.isBoardVisible()).toBeTruthy();

    // Columns mirror the [case.status] configuration.
    expect(await board.columnExists('Triage')).toBeTruthy();
    expect(await board.columnExists('In Review')).toBeTruthy();
    expect(await board.columnExists('Done')).toBeTruthy();

    // The board lists Cases, so it offers no "New Action" button; Actions are
    // managed from each case's detail page.
    expect(await board.hasNewActionButton()).toBeFalsy();

    // The sidebar nav item is labelled "Board" (not "Actions") in thread mode.
    const sidebar = page.locator('.h-side');
    await expect(sidebar.getByText('Board', { exact: true })).toBeVisible();
    await expect(sidebar.getByText('Actions', { exact: true })).toHaveCount(0);
  });

  test('still renders the Action board for a channel-mode workspace', async ({ page }) => {
    const actions = new ActionListPage(page);
    await actions.navigate(CHANNEL_WS);
    await actions.waitForBoardLoad();

    // The channel-mode workspace keeps the Action board (action testid).
    expect(await actions.isPageLoaded()).toBeTruthy();

    // And the thread-mode Case board must NOT be present here.
    const caseBoard = new CaseKanbanPage(page);
    expect(await caseBoard.isBoardVisible()).toBeFalsy();

    // The sidebar keeps the "Actions" label for channel-mode workspaces.
    const sidebar = page.locator('.h-side');
    await expect(sidebar.getByText('Actions', { exact: true })).toBeVisible();
  });

  test('New-case form hides the Private toggle in thread mode but shows it in channel mode', async ({ page }) => {
    const caseList = new CaseListPage(page);
    const caseForm = new CaseFormPage(page);

    // Thread-mode workspace: the Private toggle must never be exposed, because
    // private cases are a channel-mode-only feature (they create a dedicated
    // private channel, which thread-mode cases do not have).
    await caseList.navigate(THREAD_WS);
    await caseList.waitForTableLoad();
    await caseList.clickNewCaseButton();
    await caseForm.waitForFormVisible();
    await expect(page.getByTestId('private-case-checkbox')).toHaveCount(0);
    await caseForm.cancel();

    // Channel-mode workspace: the Private toggle is available on the create form.
    await caseList.navigate(CHANNEL_WS);
    await caseList.waitForTableLoad();
    await caseList.clickNewCaseButton();
    await caseForm.waitForFormVisible();
    await expect(page.getByTestId('private-case-checkbox')).toBeVisible();
    await caseForm.cancel();
  });

  test('case detail in a thread-mode workspace: editable board status, no Close button', async ({ page }) => {
    const caseList = new CaseListPage(page);
    const caseForm = new CaseFormPage(page);
    const detail = new CaseDetailPage(page);

    // Create a case in the thread-mode workspace, then open its detail.
    await caseList.navigate(THREAD_WS);
    await caseList.waitForTableLoad();
    await caseList.clickNewCaseButton();
    await caseForm.createCase({ title: 'Thread Detail Case', description: 'detail behaviour' });
    await caseList.waitForTableLoad();
    await caseList.fillSearchFilter('Thread Detail Case');
    await caseList.clickCaseByTitle('Thread Detail Case');
    expect(await detail.isPageLoaded()).toBeTruthy();

    // The /actions route of a thread-mode workspace is the Case board, so the
    // Related Actions section offers no "open in action list" link.
    await expect(page.getByTestId('case-open-in-actions')).toHaveCount(0);

    // close button removed: thread mode closes via the board status, not a lifecycle button.
    await expect(page.getByTestId('close-case-button')).toHaveCount(0);

    // bug 2: status is an editable board-status selector mapped to [case.status],
    // and changing it persists (no longer the read-only lifecycle Open/Closed badge).
    const status = page.getByTestId('aside-board-status');
    await expect(status).toBeVisible();
    await status.selectOption('in_review');
    await expect(status).toHaveValue('in_review');
  });

  test('a thread-mode case manages Actions from its detail page', async ({ page }) => {
    const caseList = new CaseListPage(page);
    const caseForm = new CaseFormPage(page);
    const detail = new CaseDetailPage(page);
    const actionForm = new ActionFormPage(page);
    const actionDetail = new ActionDetailPage(page);
    const caseTitle = `Thread Action Case ${Date.now()}`;
    const actionTitle = `${caseTitle} Action`;

    await caseList.navigate(THREAD_WS);
    await caseList.waitForTableLoad();
    await caseList.clickNewCaseButton();
    await caseForm.createCase({ title: caseTitle, description: 'actions in thread mode' });
    await caseList.waitForTableLoad();
    await caseList.fillSearchFilter(caseTitle);
    await caseList.clickCaseByTitle(caseTitle);
    expect(await detail.isPageLoaded()).toBeTruthy();

    // Create: the Related Actions section is available in thread mode.
    await page.getByTestId('add-action-button').click();
    await actionForm.createAction({ title: actionTitle, caseTitle });
    const actionLink = page.locator('a').filter({ hasText: actionTitle }).first();
    await expect(actionLink).toBeVisible();

    // Update: change the status and confirm it round-trips after a reload.
    await actionLink.click();
    await expect(page).toHaveURL(/\/actions\/\d+$/);
    await actionDetail.waitForPageLoad();
    await actionDetail.changeStatus('IN_PROGRESS');
    await page.reload();
    await actionDetail.waitForPageLoad();
    expect(await actionDetail.getStatus()).toBe('IN_PROGRESS');
    await actionDetail.closeModal();
    await expect(page).toHaveURL(/\/cases\/\d+$/);
    await expect(page.locator('a').filter({ hasText: actionTitle }).first()).toBeVisible();

    // The thread-mode board still lists Cases, not Actions.
    const board = new CaseKanbanPage(page);
    await board.navigate(THREAD_WS);
    await board.waitForBoardLoad();
    expect(await board.isBoardVisible()).toBeTruthy();
    expect(await board.hasNewActionButton()).toBeFalsy();
  });

  test('Case list Status column shows the configured board status in thread mode', async ({ page }) => {
    const caseList = new CaseListPage(page);
    const caseForm = new CaseFormPage(page);
    const detail = new CaseDetailPage(page);

    await caseList.navigate(THREAD_WS);
    await caseList.waitForTableLoad();
    await caseList.clickNewCaseButton();
    await caseForm.createCase({ title: 'Board Status Row', description: 'status column' });
    await caseList.waitForTableLoad();

    // The harness boots without Slack, so the thread binding that assigns the
    // initial board status never runs and the row falls back to the lifecycle
    // badge. Setting the status from the detail page is what a user does
    // anyway, and it is what the list must then reflect.
    await caseList.fillSearchFilter('Board Status Row');
    expect(await caseList.getCaseRowCellTextByHeader('Board Status Row', 'Status')).toBe('Open');

    await caseList.clickCaseByTitle('Board Status Row');
    expect(await detail.isPageLoaded()).toBeTruthy();
    const status = page.getByTestId('aside-board-status');
    await status.selectOption('in_review');
    await expect(status).toHaveValue('in_review');

    await caseList.navigate(THREAD_WS);
    await caseList.waitForTableLoad();
    await caseList.fillSearchFilter('Board Status Row');
    await expect(
      caseList.getCaseRowByTitle('Board Status Row').getByTestId('board-status-badge'),
    ).toHaveText('In Review');

    // The Slack column is not asserted here: without Slack every case has an
    // empty slackChannelID, so the cell is always a dash. The link target is
    // covered by the slackLink and CaseList unit tests instead.
  });

  test('Case list Status column keeps the lifecycle badge in channel mode', async ({ page }) => {
    const caseList = new CaseListPage(page);
    const caseForm = new CaseFormPage(page);

    await caseList.navigate(CHANNEL_WS);
    await caseList.waitForTableLoad();
    await caseList.clickNewCaseButton();
    await caseForm.createCase({
      title: 'Lifecycle Status Row',
      description: 'status column',
      customFields: { category: 'bug' },
    });
    await caseList.waitForTableLoad();
    await caseList.fillSearchFilter('Lifecycle Status Row');
    expect(await caseList.getCaseRowCellTextByHeader('Lifecycle Status Row', 'Status')).toBe('Open');
    await expect(page.getByTestId('board-status-badge')).toHaveCount(0);
  });
});

test('Case board uses the same compound field-filter links as the list', async ({ page }) => {
  const list = new CaseListPage(page);
  const form = new CaseFormPage(page);
  const filters = new CaseFieldFilters(page);
  const prefix = `Board fields ${Date.now()}`;
  await list.navigate(THREAD_WS);
  for (const [reviewType, priority] of [['code', 'high'], ['code', 'low'], ['design', 'high'], ['design', 'low'], ['design', 'medium']]) {
    const title = `${prefix} ${reviewType} ${priority}`;
    await list.clickNewCaseButton();
    await form.createCase({ title, customFields: { review_type: reviewType, priority } });
    await list.fillSearchFilter(title);
    await list.clickCaseByTitle(title);
    // Without Slack, a newly created case needs an explicit board status.
    await page.getByTestId('aside-board-status').selectOption('in_review');
    await list.navigate(THREAD_WS);
  }
  await page.goto(`/ws/${THREAD_WS}/actions?field.review_type=code&field.priority=high`);
  const cards = page.getByTestId('case-card');
  await expect(cards.filter({ hasText: `${prefix} code high` })).toBeVisible();
  await expect(cards.filter({ hasText: `${prefix} code low` })).toHaveCount(0);
  await expect(cards.filter({ hasText: `${prefix} design high` })).toHaveCount(0);
  await filters.toggleOption('review_type', 'Design');
  await filters.toggleOption('priority', 'Low');
  await filters.close();
  await expect(cards.filter({ hasText: `${prefix} code low` })).toBeVisible();
  await expect(cards.filter({ hasText: `${prefix} design low` })).toBeVisible();
  await expect(cards.filter({ hasText: `${prefix} design medium` })).toHaveCount(0);
  const shared = new URL(page.url()).search;
  await page.reload();
  await expect(cards.filter({ hasText: `${prefix} design low` })).toBeVisible();
  await expect(page.getByTestId('case-field-filters-summary')).toContainText('Low');
  await page.goto(`/ws/${THREAD_WS}/cases${shared}`);
  await expect(list.getCaseRowByTitle(`${prefix} code high`)).toBeVisible();
  await expect(list.getCaseRowByTitle(`${prefix} design low`)).toBeVisible();
  await expect(list.getCaseRowByTitle(`${prefix} design medium`)).toHaveCount(0);
  await filters.clear();
  await expect(list.getCaseRowByTitle(`${prefix} design medium`)).toBeVisible();
  await expect(page).not.toHaveURL(/field\./);
});
