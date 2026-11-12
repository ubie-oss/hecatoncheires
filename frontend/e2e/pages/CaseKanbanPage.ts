import { Page, Locator } from '@playwright/test';
import { BasePage } from './BasePage';

/**
 * Page Object for the thread-mode Case Kanban board, served at /ws/:id/board.
 * The workspace's /actions route is the Action board when the workspace has
 * Actions, and forwards to /board when it does not.
 */
export class CaseKanbanPage extends BasePage {
  private readonly board: Locator;
  private readonly searchInput: Locator;
  private readonly newActionButton: Locator;

  constructor(page: Page) {
    super(page);
    this.board = page.getByTestId('case-kanban-board');
    this.searchInput = page.getByTestId('case-board-search-input');
    // The action-board "New Action" button must NOT appear in thread mode.
    this.newActionButton = page.locator('button').filter({ hasText: /New Action/ });
  }

  async navigate(workspaceId: string): Promise<void> {
    await this.goto(`/ws/${workspaceId}/board`);
  }

  async waitForBoardLoad(): Promise<void> {
    await this.board.waitFor({ state: 'visible', timeout: 10000 });
    await this.page.evaluate(() => new Promise((resolve) => requestAnimationFrame(resolve)));
  }

  async isBoardVisible(): Promise<boolean> {
    try {
      await this.board.waitFor({ state: 'visible', timeout: 5000 });
      return true;
    } catch {
      return false;
    }
  }

  async hasNewActionButton(): Promise<boolean> {
    return this.newActionButton.isVisible().catch(() => false);
  }

  private slug(columnTitle: string): string {
    return columnTitle.toLowerCase().trim().replace(/[\s_]+/g, '-');
  }

  async columnExists(columnTitle: string): Promise<boolean> {
    try {
      await this.page
        .getByTestId(`case-kanban-column-${this.slug(columnTitle)}`)
        .waitFor({ state: 'visible', timeout: 5000 });
      return true;
    } catch {
      return false;
    }
  }

  async hasSearchInput(): Promise<boolean> {
    return this.searchInput.isVisible().catch(() => false);
  }
}
