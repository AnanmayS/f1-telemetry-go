import { defineConfig } from '@playwright/test';
import base from './playwright.config';

// Regenerates web/screenshots/*.png (npm run shots). Kept out of `npm test`.
export default defineConfig({ ...base, testMatch: '*.shots.ts' });
