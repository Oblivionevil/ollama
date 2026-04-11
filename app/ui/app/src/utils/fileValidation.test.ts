import { describe, it, expect } from "vitest";
import {
  IMAGE_EXTENSIONS,
  TEXT_FILE_EXTENSIONS,
  validateFile,
} from "./fileValidation";

describe("fileValidation", () => {
  describe("IMAGE_EXTENSIONS", () => {
    it("should include all supported image formats including WebP", () => {
      expect(IMAGE_EXTENSIONS).toContain("png");
      expect(IMAGE_EXTENSIONS).toContain("jpg");
      expect(IMAGE_EXTENSIONS).toContain("jpeg");
      expect(IMAGE_EXTENSIONS).toContain("webp");
    });
  });

  describe("TEXT_FILE_EXTENSIONS", () => {
    it("should include common office and document formats", () => {
      expect(TEXT_FILE_EXTENSIONS).toContain("docx");
      expect(TEXT_FILE_EXTENSIONS).toContain("docm");
      expect(TEXT_FILE_EXTENSIONS).toContain("xlsx");
      expect(TEXT_FILE_EXTENSIONS).toContain("xlsm");
      expect(TEXT_FILE_EXTENSIONS).toContain("pptx");
      expect(TEXT_FILE_EXTENSIONS).toContain("pptm");
      expect(TEXT_FILE_EXTENSIONS).toContain("odt");
      expect(TEXT_FILE_EXTENSIONS).toContain("ods");
      expect(TEXT_FILE_EXTENSIONS).toContain("odp");
      expect(TEXT_FILE_EXTENSIONS).toContain("rtf");
      expect(TEXT_FILE_EXTENSIONS).toContain("tsv");
      expect(TEXT_FILE_EXTENSIONS).toContain("xhtml");
    });
  });

  describe("validateFile", () => {
    const createMockFile = (
      name: string,
      size: number,
      type: string,
    ): File => {
      const blob = new Blob(["test content"], { type });
      return new File([blob], name, { type });
    };

    it("should accept WebP images when vision capability is enabled", () => {
      const file = createMockFile("test.webp", 1024, "image/webp");
      const result = validateFile(file, {
        hasVisionCapability: true,
      });
      expect(result.valid).toBe(true);
    });

    it("should reject WebP images when vision capability is disabled", () => {
      const file = createMockFile("test.webp", 1024, "image/webp");
      const result = validateFile(file, {
        hasVisionCapability: false,
      });
      expect(result.valid).toBe(false);
      expect(result.error).toBe("This model does not support images");
    });

    it("should accept PNG images when vision capability is enabled", () => {
      const file = createMockFile("test.png", 1024, "image/png");
      const result = validateFile(file, {
        hasVisionCapability: true,
      });
      expect(result.valid).toBe(true);
    });

    it("should accept JPEG images when vision capability is enabled", () => {
      const file = createMockFile("test.jpg", 1024, "image/jpeg");
      const result = validateFile(file, {
        hasVisionCapability: true,
      });
      expect(result.valid).toBe(true);
    });

    it("should reject files that are too large", () => {
      // Create a file with size property set correctly
      const largeSize = 11 * 1024 * 1024; // 11MB
      const content = new Uint8Array(largeSize);
      const blob = new Blob([content], { type: "image/webp" });
      const file = new File([blob], "large.webp", { type: "image/webp" });
      
      const result = validateFile(file, {
        hasVisionCapability: true,
        maxFileSize: 10, // 10MB limit
      });
      expect(result.valid).toBe(false);
      expect(result.error).toBe("File too large");
    });

    it("should reject unsupported file types", () => {
      const file = createMockFile("test.xyz", 1024, "application/xyz");
      const result = validateFile(file, {
        hasVisionCapability: true,
      });
      expect(result.valid).toBe(false);
      expect(result.error).toBe("File type not supported");
    });

    it("should respect custom validators", () => {
      const file = createMockFile("test.webp", 1024, "image/webp");
      const result = validateFile(file, {
        hasVisionCapability: true,
        customValidator: () => ({
          valid: false,
          error: "Custom error",
        }),
      });
      expect(result.valid).toBe(false);
      expect(result.error).toBe("Custom error");
    });
  });

  // Note: processFiles tests are skipped because FileReader is not available in the Node.js test environment
  // These functions are tested in browser environment via integration tests
});
