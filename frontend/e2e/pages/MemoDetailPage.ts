import { Page, Locator } from '@playwright/test';
import { BasePage } from './BasePage';

/**
 * Page Object for the memo detail page
 * (`/ws/{workspaceId}/cases/{caseId}/memos/{memoId}`).
 *
 * The page hosts the edit form (MemoFormModal) and the archive confirm
 * dialog (MemoArchiveDialog), so their controls are exposed here too.
 */
export class MemoDetailPage extends BasePage {
  readonly root: Locator;
  readonly title: Locator;
  readonly backLink: Locator;
  readonly editButton: Locator;
  readonly archiveButton: Locator;
  readonly unarchiveButton: Locator;
  readonly archiveConfirmButton: Locator;
  readonly formTitleInput: Locator;
  readonly formSubmitButton: Locator;

  constructor(page: Page) {
    super(page);
    this.root = page.getByTestId('memo-detail-page');
    this.title = page.getByTestId('memo-detail-title');
    this.backLink = page.getByTestId('memo-detail-back-link');
    this.editButton = page.getByTestId('memo-detail-edit-button');
    this.archiveButton = page.getByTestId('memo-detail-archive-button');
    this.unarchiveButton = page.getByTestId('memo-detail-unarchive-button');
    this.archiveConfirmButton = page.getByTestId('memo-archive-confirm-button');
    this.formTitleInput = page.locator('#memo-title');
    this.formSubmitButton = page.getByTestId('memo-form-submit');
  }

  async waitForLoaded(): Promise<void> {
    await this.waitForVisible(this.root);
  }

  /** Archive the memo through the confirm dialog. */
  async archive(): Promise<void> {
    await this.archiveButton.click();
    await this.archiveConfirmButton.click();
    // The page flips to the archived state in place.
    await this.waitForVisible(this.unarchiveButton);
  }

  /** Restore an archived memo; resolves when the active actions are back. */
  async unarchive(): Promise<void> {
    await this.unarchiveButton.click();
    await this.waitForVisible(this.editButton);
  }

  /** Open the edit modal, replace the title, and save. */
  async editTitle(newTitle: string): Promise<void> {
    await this.editButton.click();
    await this.waitForVisible(this.formTitleInput);
    await this.formTitleInput.fill(newTitle);
    await this.formSubmitButton.click();
    await this.waitForHidden(this.formSubmitButton);
  }

  /** The notice shown on a memo whose case is archived. */
  caseArchivedNotice(): Locator {
    return this.page.getByTestId('memo-detail-case-archived-notice');
  }

  async backToCase(): Promise<void> {
    await this.backLink.click();
  }
}
