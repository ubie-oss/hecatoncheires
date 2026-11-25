import { test, expect, type Page } from '@playwright/test';
import { CaseListPage } from '../pages/CaseListPage';
import { CaseFormPage } from '../pages/CaseFormPage';
import { CaseDetailPage } from '../pages/CaseDetailPage';
import { ActionFormPage } from '../pages/ActionFormPage';
import { ActionDetailPage } from '../pages/ActionDetailPage';
import { MemoDetailPage } from '../pages/MemoDetailPage';
import { TEST_WORKSPACE_ID } from '../fixtures/testData';

// An archived object is read-only: the server rejects every content write to
// an archived case (and to the actions, steps, comments and memos under it),
// to an archived action, and to an archived memo. These journeys check that
// the UI offers none of those writes while archived, says why, and becomes
// editable again once the object is restored.
function uniq(): string {
  return `${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
}

// Create a case holding one memo and one action, leaving the browser on the
// case detail page.
async function setupCase(page: Page, caseTitle: string, memoTitle: string, actionTitle: string): Promise<void> {
  const caseListPage = new CaseListPage(page);
  const caseFormPage = new CaseFormPage(page);
  const caseDetailPage = new CaseDetailPage(page);
  const actionFormPage = new ActionFormPage(page);

  await caseListPage.navigate(TEST_WORKSPACE_ID);
  await caseListPage.waitForTableLoad();
  await caseListPage.clickNewCaseButton();
  await caseFormPage.createCase({ title: caseTitle, customFields: { category: 'task' } });
  await caseListPage.waitForTableLoad();
  await caseListPage.fillSearchFilter(caseTitle);
  await caseListPage.clickCaseByTitle(caseTitle);
  await caseDetailPage.waitForPageLoad();

  await page.getByTestId('new-memo-button').click();
  await page.locator('#memo-title').fill(memoTitle);
  await page.getByTestId('memo-form-submit').click();
  await expect(page.getByTestId('memo-row').filter({ hasText: memoTitle })).toBeVisible();

  await page.getByTestId('empty-add-action-button').click();
  await actionFormPage.createAction({ title: actionTitle });
  await expect(caseDetailPage.relatedActionByTitle(actionTitle)).toBeVisible();
}

test.describe('Archived objects are read-only', () => {
  test('an archived case, its memos and its actions cannot be edited until it is restored', async ({ page }) => {
    const caseDetailPage = new CaseDetailPage(page);
    const actionDetailPage = new ActionDetailPage(page);
    const memoDetailPage = new MemoDetailPage(page);

    const caseTitle = `Archive RO Case ${uniq()}`;
    const memoTitle = `Archive RO Memo ${uniq()}`;
    const actionTitle = `Archive RO Action ${uniq()}`;
    await setupCase(page, caseTitle, memoTitle, actionTitle);

    // Before archiving everything is editable.
    await expect(caseDetailPage.readOnlyNotice()).toHaveCount(0);
    await expect(caseDetailPage.titleFrame()).not.toHaveAttribute('aria-disabled', 'true');

    await caseDetailPage.clickCloseButton();
    await caseDetailPage.clickArchive();
    await expect(caseDetailPage.archivedBadge()).toBeVisible();

    // The case page: a notice, disabled editors, nothing to add.
    await expect(caseDetailPage.readOnlyNotice()).toBeVisible();
    await expect(caseDetailPage.readOnlyNotice()).toContainText('is archived and cannot be edited');
    await expect(caseDetailPage.titleFrame()).toHaveAttribute('aria-disabled', 'true');
    // Playwright refuses to click an aria-disabled element; force the click to
    // prove it opens no editor.
    await caseDetailPage.titleFrame().click({ force: true });
    await expect(page.getByTestId('case-title-input')).toHaveCount(0);
    await expect(page.getByTestId('case-description')).toHaveAttribute('aria-disabled', 'true');
    await expect(page.getByTestId('test-case-toggle')).toBeDisabled();
    await expect(page.getByTestId('add-action-button')).toHaveCount(0);
    await expect(page.getByTestId('new-memo-button')).toHaveCount(0);
    // The existing memo and action are still listed.
    await expect(page.getByTestId('memo-row').filter({ hasText: memoTitle })).toBeVisible();
    await expect(caseDetailPage.relatedActionByTitle(actionTitle)).toBeVisible();

    // The action under the archived case is read-only in its modal.
    await caseDetailPage.relatedActionByTitle(actionTitle).click();
    await actionDetailPage.waitForPageLoad();
    await expect(actionDetailPage.readOnlyNotice()).toContainText('belongs to is archived');
    await expect(page.getByTestId('action-title')).toHaveAttribute('aria-disabled', 'true');
    await expect(page.getByTestId('status-dropdown')).toBeDisabled();
    await expect(actionDetailPage.stepList().getByTestId('action-step-add-button')).toHaveCount(0);
    await expect(actionDetailPage.commentComposer()).toHaveCount(0);
    await actionDetailPage.closeModal();

    // The memo of the archived case offers no Edit and says why.
    await page.getByTestId('memo-row').filter({ hasText: memoTitle }).click();
    await memoDetailPage.waitForLoaded();
    await expect(memoDetailPage.caseArchivedNotice()).toBeVisible();
    await expect(memoDetailPage.editButton).toHaveCount(0);
    await memoDetailPage.backToCase();
    await caseDetailPage.waitForPageLoad();

    // Restoring makes the case editable again, and an edit round-trips.
    await caseDetailPage.clickUnarchive();
    await expect(caseDetailPage.archivedBadge()).toHaveCount(0);
    await expect(caseDetailPage.readOnlyNotice()).toHaveCount(0);
    const renamed = `${caseTitle} renamed`;
    await caseDetailPage.setTitle(renamed);
    await expect(caseDetailPage.titleFrame()).toContainText(renamed);
    await page.reload();
    await caseDetailPage.waitForPageLoad();
    await expect(caseDetailPage.titleFrame()).toContainText(renamed);
    await expect(page.getByTestId('new-memo-button')).toBeVisible();
  });

  test('an archived action cannot be edited until it is restored', async ({ page }) => {
    const caseDetailPage = new CaseDetailPage(page);
    const actionDetailPage = new ActionDetailPage(page);

    const caseTitle = `Archive RO Parent ${uniq()}`;
    const actionTitle = `Archive RO Only Action ${uniq()}`;
    await setupCase(page, caseTitle, `Archive RO Side Memo ${uniq()}`, actionTitle);

    // Give the action a step and a comment, then archive it.
    await caseDetailPage.relatedActionByTitle(actionTitle).click();
    await actionDetailPage.waitForPageLoad();
    await actionDetailPage.addStep('Collect logs');
    await expect(actionDetailPage.readOnlyNotice()).toHaveCount(0);
    await expect(actionDetailPage.commentComposer()).toBeVisible();
    await actionDetailPage.archive();

    // Open it again from the Archived view.
    await page.getByTestId('action-view-archived').click();
    await caseDetailPage.relatedActionByTitle(actionTitle).click();
    await actionDetailPage.waitForPageLoad();
    await expect(actionDetailPage.readOnlyNotice()).toContainText('This action is archived');
    await expect(page.getByTestId('action-title')).toHaveAttribute('aria-disabled', 'true');
    const step = actionDetailPage.stepRowByTitle('Collect logs');
    await expect(step).toBeVisible();
    await expect(step.locator('[data-testid^="action-step-checkbox-"]')).toBeDisabled();
    await expect(step.locator('[data-testid^="action-step-delete-"]')).toHaveCount(0);
    await expect(actionDetailPage.stepList().getByTestId('action-step-add-button')).toHaveCount(0);
    await expect(actionDetailPage.commentComposer()).toHaveCount(0);

    // Restore it; it is editable again and an edit persists.
    await actionDetailPage.unarchive();
    // The case page's Active list is not refetched by the modal's mutation,
    // so reload to read the restored action from the server.
    await page.reload();
    await caseDetailPage.waitForPageLoad();
    await caseDetailPage.relatedActionByTitle(actionTitle).click();
    await actionDetailPage.waitForPageLoad();
    await expect(actionDetailPage.readOnlyNotice()).toHaveCount(0);
    await actionDetailPage.toggleStep('Collect logs');
    await expect.poll(() => actionDetailPage.isStepDone('Collect logs')).toBeTruthy();
    await page.reload();
    await actionDetailPage.waitForPageLoad();
    expect(await actionDetailPage.isStepDone('Collect logs')).toBeTruthy();
  });
});
