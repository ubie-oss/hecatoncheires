import { Page, Locator, expect } from '@playwright/test';
import { BasePage } from './BasePage';

/**
 * Page Object for the Action Modal (detail/edit/delete)
 */
export class ActionDetailPage extends BasePage {
  // Locators
  private readonly modalTitle: Locator;
  private readonly deleteButton: Locator;
  private readonly loadingIndicator: Locator;

  constructor(page: Page) {
    super(page);
    this.modalTitle = page.locator('#modal-title');
    this.deleteButton = page.locator('button').filter({ hasText: /Delete/ }).first();
    this.loadingIndicator = page.locator('text=Loading...');
  }

  /**
   * Navigate to a specific action via permalink (opens kanban + modal)
   */
  async navigate(workspaceId: string, actionId: number): Promise<void> {
    await this.goto(`/ws/${workspaceId}/actions/${actionId}`);
  }

  /**
   * Get the action title displayed in the modal body
   */
  async getTitle(): Promise<string> {
    // Multiple elements share class names like "titleText" (kanban cards + modal h3),
    // so target the modal title via its dedicated test id.
    const titleText = this.page.getByTestId('action-title');
    await titleText.waitFor({ state: 'visible', timeout: 5000 });
    return await titleText.textContent() || '';
  }

  /**
   * Inline-edit the action title (Linear-style: click → input → Enter).
   */
  async setTitle(next: string): Promise<void> {
    await this.page.getByTestId('action-title').click();
    const input = this.page.getByTestId('action-title-input');
    await input.waitFor({ state: 'visible', timeout: 3000 });
    await input.fill(next);
    await input.press('Enter');
  }

  /**
   * Inline-edit the action description.
   */
  async setDescription(next: string): Promise<void> {
    await this.page.getByTestId('action-description').click();
    const input = this.page.getByTestId('action-description-input');
    await input.waitFor({ state: 'visible', timeout: 3000 });
    await input.fill(next);
    await this.page.getByTestId('action-description-save').click();
  }

  /**
   * Click the Delete button in the modal
   */
  async clickDelete(): Promise<void> {
    await this.deleteButton.click();
  }

  /**
   * Wait for the modal to load
   */
  async waitForPageLoad(): Promise<void> {
    await this.loadingIndicator.waitFor({ state: 'hidden', timeout: 10000 }).catch(() => {});
    await this.modalTitle.waitFor({ state: 'visible', timeout: 10000 });
  }

  /**
   * Check if the modal is loaded
   */
  async isPageLoaded(): Promise<boolean> {
    try {
      await this.modalTitle.waitFor({ state: 'visible', timeout: 5000 });
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Get the current status value from the hidden dropdown
   */
  async getStatus(): Promise<string> {
    const dropdown = this.page.getByTestId('status-dropdown');
    return await dropdown.inputValue();
  }

  /**
   * Change the action status via the hidden dropdown
   */
  async changeStatus(newStatus: string): Promise<void> {
    const dropdown = this.page.getByTestId('status-dropdown');
    const responsePromise = this.page.waitForResponse(
      (resp) => resp.url().includes('/graphql') && resp.status() === 200
    );
    await dropdown.selectOption(newStatus);
    await responsePromise;
    // The mutation response settles the network, but the controlled <select>
    // only reflects the new value after Apollo normalises the cache and React
    // re-renders. Wait for that so callers reading getStatus() don't race.
    await expect(dropdown).toHaveValue(newStatus, { timeout: 5000 });
  }

  // --- Action steps (checklist) ---

  /** The step-list section within the action modal. */
  stepList(): Locator {
    return this.page.getByTestId('action-step-list');
  }

  /** A step row located by its title text. */
  stepRowByTitle(title: string): Locator {
    return this.stepList()
      .locator('[data-testid^="action-step-row-"]')
      .filter({ hasText: title });
  }

  /** The done/total progress pill text (e.g. "1/2"); empty when no steps. */
  async getStepProgress(): Promise<string> {
    const pill = this.stepList().getByTestId('action-step-progress');
    if (!(await pill.count())) return '';
    return (await pill.textContent()) || '';
  }

  /** Add a step through the inline add-row and wait for it to render. */
  async addStep(title: string): Promise<void> {
    await this.stepList().getByTestId('action-step-add-button').click();
    const input = this.stepList().getByTestId('action-step-add-input');
    await input.fill(title);
    await input.press('Enter');
    await expect(this.stepRowByTitle(title)).toBeVisible();
  }

  /** Toggle a step's done checkbox by its title. */
  async toggleStep(title: string): Promise<void> {
    await this.stepRowByTitle(title)
      .locator('[data-testid^="action-step-checkbox-"]')
      .click();
  }

  /** Whether the step with the given title is marked done. */
  async isStepDone(title: string): Promise<boolean> {
    const checked = await this.stepRowByTitle(title)
      .locator('[data-testid^="action-step-checkbox-"]')
      .getAttribute('aria-checked');
    return checked === 'true';
  }

  /** Delete a step by its title (hover reveals the row's delete control). */
  async deleteStep(title: string): Promise<void> {
    const row = this.stepRowByTitle(title);
    await row.hover();
    await row.locator('[data-testid^="action-step-delete-"]').click();
    await expect(row).toHaveCount(0);
  }

  // --- Action comments ---

  /** The activity feed section within the action modal. */
  activity(): Locator {
    return this.page.getByTestId('action-activity');
  }

  /** A comment row located by its id (returned by the GraphQL mutation). */
  commentRowById(id: string): Locator {
    return this.page.getByTestId(`activity-comment-${id}`);
  }

  /** A comment row located by its rendered body text. */
  commentRowByBody(body: string): Locator {
    return this.activity()
      .locator('[data-testid^="activity-comment-"]')
      .filter({ hasText: body });
  }

  /** Post a comment through the composer and wait for it to appear. */
  async addComment(body: string): Promise<void> {
    const textarea = this.page.getByTestId('action-comment-body-textarea');
    await textarea.waitFor({ state: 'visible', timeout: 5000 });
    await textarea.fill(body);
    await this.page.getByTestId('action-comment-submit').click();
    await expect(this.commentRowByBody(body)).toBeVisible({ timeout: 10000 });
  }

  /**
   * Rewrite an existing comment, located by its current body text.
   *
   * The row is re-located by its own test id before the edit starts: opening
   * the editor replaces the rendered body with a textarea, so a locator that
   * filters on the old body text stops matching the moment the new text is
   * typed — and the save button inside it becomes unreachable.
   */
  async editComment(currentBody: string, nextBody: string): Promise<void> {
    const testId = await this.commentRowByBody(currentBody).getAttribute('data-testid');
    expect(testId).not.toBeNull();
    const row = this.page.getByTestId(testId as string);

    await row.locator('[data-testid^="action-comment-edit-"]').click();
    const textarea = row.locator('textarea');
    await textarea.waitFor({ state: 'visible', timeout: 5000 });
    await textarea.fill(nextBody);
    await row.locator('[data-testid^="action-comment-save-"]').click();
    await expect(this.commentRowByBody(nextBody)).toBeVisible({ timeout: 10000 });
  }

  /** Delete a comment, located by its body text, through the confirm dialog. */
  async deleteComment(body: string): Promise<void> {
    const row = this.commentRowByBody(body);
    await row.locator('[data-testid^="action-comment-delete-"]').click();
    await this.page.getByTestId('action-comment-delete-confirm').click();
    await expect(this.commentRowByBody(body)).toHaveCount(0, { timeout: 10000 });
  }

  /** Select one of the activity feed's tabs. */
  async selectActivityTab(tab: 'all' | 'comments' | 'history'): Promise<void> {
    await this.page.getByTestId(`activity-tab-${tab}`).click();
  }

  /**
   * Whether the comment with the given body shows the "edited" marker. Matched
   * exactly so a body that happens to contain the word does not read as edited
   * (playwright.config.ts pins the browser locale to en-US, so the marker is
   * the English string).
   */
  async isCommentEdited(body: string): Promise<boolean> {
    return await this.commentRowByBody(body)
      .getByText('edited', { exact: true })
      .isVisible();
  }

  /** The notice a read-only (archived, or archived-case) action shows. */
  readOnlyNotice(): Locator {
    return this.page.getByTestId('action-read-only-notice');
  }

  /** The comment composer under the activity feed. */
  commentComposer(): Locator {
    return this.page.getByTestId('action-comment-composer');
  }

  /** Archive the action through its confirm dialog; the modal closes. */
  async archive(): Promise<void> {
    await this.page.getByTestId('action-archive-button').click();
    const responsePromise = this.page.waitForResponse(
      (resp) => resp.url().includes('/graphql') && resp.status() === 200
    );
    await this.page.getByTestId('action-archive-confirm-button').click();
    await responsePromise;
    await this.page.getByTestId('action-title').waitFor({ state: 'hidden', timeout: 5000 });
  }

  /** Restore the action through its confirm dialog; the modal closes. */
  async unarchive(): Promise<void> {
    await this.page.getByTestId('action-unarchive-button').click();
    const responsePromise = this.page.waitForResponse(
      (resp) => resp.url().includes('/graphql') && resp.status() === 200
    );
    await this.page.getByTestId('action-unarchive-confirm-button').click();
    await responsePromise;
    await this.page.getByTestId('action-title').waitFor({ state: 'hidden', timeout: 5000 });
  }

  /**
   * Close the modal by clicking the close button
   */
  async closeModal(): Promise<void> {
    // Target the modal's own close control by test id: on a Case detail page
    // there are other buttons named "Close" (closing the Case itself), and a
    // role+name lookup hits all of them.
    await this.page.getByTestId('modal-close-button').click();
    await this.page.locator('[class*="backdrop"]').waitFor({ state: 'hidden', timeout: 5000 }).catch(() => {});
  }
}
