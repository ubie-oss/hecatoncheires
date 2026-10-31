import { test, expect } from '@playwright/test';
import { CaseListPage } from '../pages/CaseListPage';
import { CaseFormPage } from '../pages/CaseFormPage';
import { TEST_WORKSPACE_ID } from '../fixtures/testData';

// E2E coverage for the Bulk draft actions feature on the Drafts tab.
//
// What this spec validates:
//   - The Drafts tab adds a checkbox column (header + per-row); other
//     tabs do not.
//   - Selecting drafts surfaces a BulkSelectionBar with Submit / Delete /
//     Clear controls and a visible count.
//   - Bulk delete requires the confirmation dialog; cancel does not
//     delete; confirm removes the selected drafts.
//   - Bulk submit promotes selected drafts to OPEN; the result dialog
//     surfaces success/failure breakdown.
//   - The header checkbox supports select-all / clear-all.

async function createDraft(
  page: import('@playwright/test').Page,
  caseListPage: CaseListPage,
  caseFormPage: CaseFormPage,
  title: string,
  // When set, fills the required `category` custom field so the resulting
  // draft is actually promotable via bulk submit. Omit to create the usual
  // half-written draft (missing required fields).
  category?: string,
): Promise<void> {
  await caseListPage.clickNewCaseButton();
  await caseFormPage.waitForFormVisible();
  await caseFormPage.fillTitle(title);
  if (category) {
    await caseFormPage.fillCustomField('category', category);
  }
  await caseFormPage.clickSaveAsDraft();
  await page.waitForURL(new RegExp(`/ws/${TEST_WORKSPACE_ID}/cases$`));
  await caseListPage.waitForTableLoad();
}

test.describe('Bulk draft actions (Drafts tab)', () => {
  test.beforeEach(async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    await caseListPage.navigate(TEST_WORKSPACE_ID);
    await caseListPage.waitForTableLoad();
  });

  // Selection is offered on the tabs that have a bulk action for their rows:
  // Drafts (submit / delete), Closed (archive) and Archived (restore). Open
  // and All have none — a bulk archive there would mix rows the operation
  // cannot apply to (archiving is CLOSED-only) with ones it can.
  test('checkbox column is present on the Drafts, Closed and Archived tabs only', async ({ page }) => {
    const caseListPage = new CaseListPage(page);

    await caseListPage.clickStatusTab('Open');
    await expect(page.getByTestId('bulk-header-checkbox')).toHaveCount(0);

    await caseListPage.clickStatusTab('All');
    await expect(page.getByTestId('bulk-header-checkbox')).toHaveCount(0);

    await caseListPage.clickStatusTab('Closed');
    await expect(page.getByTestId('bulk-header-checkbox')).toBeVisible();

    await caseListPage.clickStatusTab('Archived');
    await expect(page.getByTestId('bulk-header-checkbox')).toBeVisible();

    await caseListPage.clickStatusTab('Draft');
    await expect(page.getByTestId('bulk-header-checkbox')).toBeVisible();
  });

  test('select-all toggles row checkboxes and surfaces the bulk bar', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);

    const tag = Date.now();
    const titleA = `bulk-select-all-A ${tag}`;
    const titleB = `bulk-select-all-B ${tag}`;
    await createDraft(page, caseListPage, caseFormPage, titleA);
    await createDraft(page, caseListPage, caseFormPage, titleB);

    await caseListPage.clickStatusTab('Draft');
    // Bulk bar starts hidden.
    await expect(page.getByTestId('bulk-selection-bar')).toHaveCount(0);

    const boxA = caseListPage.getCaseRowByTitle(titleA).locator('input[type="checkbox"]');
    const boxB = caseListPage.getCaseRowByTitle(titleB).locator('input[type="checkbox"]');
    await expect(boxA).not.toBeChecked();
    await expect(boxB).not.toBeChecked();

    // Header checkbox selects every accessible draft on the current filter.
    // The exact count is workspace-wide (other tests' drafts share the tab),
    // but our two rows specifically must flip to checked — that is what
    // select-all is supposed to do.
    await page.getByTestId('bulk-header-checkbox').check();
    await expect(page.getByTestId('bulk-selection-bar')).toBeVisible();
    await expect(boxA).toBeChecked();
    await expect(boxB).toBeChecked();

    // Clear collapses the bar again and unchecks our rows.
    await page.getByTestId('bulk-clear-button').click();
    await expect(page.getByTestId('bulk-selection-bar')).toHaveCount(0);
    await expect(boxA).not.toBeChecked();
    await expect(boxB).not.toBeChecked();
  });

  test('bulk delete shows the confirm dialog; cancel does not delete', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);

    const tag = Date.now();
    const title = `bulk-delete-cancel ${tag}`;
    await createDraft(page, caseListPage, caseFormPage, title);

    await caseListPage.clickStatusTab('Draft');
    expect(await caseListPage.caseExists(title)).toBeTruthy();

    const row = caseListPage.getCaseRowByTitle(title);
    await row.locator('input[type="checkbox"]').check();

    await page.getByTestId('bulk-delete-button').click();
    await expect(page.getByTestId('bulk-delete-confirm-body')).toBeVisible();

    await page.getByTestId('bulk-delete-confirm-cancel').click();
    await expect(page.getByTestId('bulk-delete-confirm-body')).toHaveCount(0);

    // Draft still present.
    expect(await caseListPage.caseExists(title)).toBeTruthy();
  });

  test('bulk delete confirm removes the selected drafts and shows the result dialog', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);

    const tag = Date.now();
    const t1 = `bulk-delete-A ${tag}`;
    const t2 = `bulk-delete-B ${tag}`;
    await createDraft(page, caseListPage, caseFormPage, t1);
    await createDraft(page, caseListPage, caseFormPage, t2);

    await caseListPage.clickStatusTab('Draft');
    await caseListPage.getCaseRowByTitle(t1).locator('input[type="checkbox"]').check();
    await caseListPage.getCaseRowByTitle(t2).locator('input[type="checkbox"]').check();

    await page.getByTestId('bulk-delete-button').click();
    await page.getByTestId('bulk-delete-confirm-confirm').click();

    // Result dialog renders summary + success section.
    await expect(page.getByTestId('bulk-result-dialog-body')).toBeVisible();
    await expect(page.getByTestId('bulk-result-summary')).toContainText('2');
    await page.getByTestId('bulk-result-close-button').click();

    // Refetch already fired; drafts are gone from the list.
    await caseListPage.waitForTableLoad();
    expect(await caseListPage.caseExists(t1)).toBeFalsy();
    expect(await caseListPage.caseExists(t2)).toBeFalsy();
  });

  test('bulk submit promotes the selected drafts to OPEN', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);

    const tag = Date.now();
    const draftTitle = `bulk-submit ${tag}`;
    // Fill the required `category` so the draft is genuinely promotable —
    // otherwise SubmitDraft would fail validation and the assertion below
    // could not distinguish a working feature from a broken one.
    await createDraft(page, caseListPage, caseFormPage, draftTitle, 'bug');

    await caseListPage.clickStatusTab('Draft');
    await caseListPage.getCaseRowByTitle(draftTitle).locator('input[type="checkbox"]').check();
    await page.getByTestId('bulk-submit-button').click();

    // The result dialog must report exactly one success and no failures.
    await expect(page.getByTestId('bulk-result-dialog-body')).toBeVisible();
    await expect(page.getByTestId('bulk-result-summary')).toContainText('1');
    const successList = page.getByTestId('bulk-result-success-list');
    await expect(successList).toBeVisible();
    await expect(successList.getByText(draftTitle)).toBeVisible();
    await expect(page.getByTestId('bulk-result-failure-list')).toHaveCount(0);
    await page.getByTestId('bulk-result-close-button').click();

    // Ground truth: the promoted case now lives on the Open tab and is gone
    // from the Drafts tab. Reload first so the tab lists are read fresh from
    // the backend — Apollo's cache-first policy would otherwise serve an
    // Open-tab list captured before the promotion. Scope by the unique title
    // so the client-side 20-row pagination can't hide it behind other data.
    await page.reload();
    await caseListPage.waitForTableLoad();
    await caseListPage.clickStatusTab('Open');
    await caseListPage.fillSearchFilter(draftTitle);
    expect(await caseListPage.caseExists(draftTitle)).toBeTruthy();

    await caseListPage.clickStatusTab('Draft');
    await caseListPage.fillSearchFilter(draftTitle);
    expect(await caseListPage.caseExists(draftTitle)).toBeFalsy();
  });

  // The server rejects SubmitDraft with HTTP 400 and data: null, so the
  // MISSING_REQUIRED_FIELDS code reaches the client only through the error
  // response body, not through Apollo's graphQLErrors.
  test('bulk submit of a draft missing a required field names the field', async ({ page }) => {
    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);

    const tag = Date.now();
    const draftTitle = `bulk-submit-missing ${tag}`;
    await createDraft(page, caseListPage, caseFormPage, draftTitle);

    await caseListPage.clickStatusTab('Draft');
    await caseListPage.getCaseRowByTitle(draftTitle).locator('input[type="checkbox"]').check();
    await page.getByTestId('bulk-submit-button').click();

    await expect(page.getByTestId('bulk-result-dialog-body')).toBeVisible();
    const failureList = page.getByTestId('bulk-result-failure-list');
    await expect(failureList).toContainText(draftTitle);
    await expect(failureList).toContainText(/Required fields are missing: .*category/i);
    await expect(failureList).not.toContainText('Network error');
    await page.getByTestId('bulk-result-close-button').click();

    // The draft was not promoted.
    await page.reload();
    await caseListPage.waitForTableLoad();
    await caseListPage.clickStatusTab('Draft');
    await caseListPage.fillSearchFilter(draftTitle);
    expect(await caseListPage.caseExists(draftTitle)).toBeTruthy();
  });
});
