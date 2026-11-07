export class AppError extends Error {
  constructor(
    message: string,
    readonly code: string,
    readonly statusCode = 400,
    options?: ErrorOptions,
  ) {
    super(message, options);
    this.name = new.target.name;
  }
}

export class NotFoundError extends AppError {
  constructor(message: string) {
    super(message, "NOT_FOUND", 404);
  }
}

export class PolicyError extends AppError {
  constructor(message: string) {
    super(message, "POLICY_DENIED", 403);
  }
}

export class AuthenticationError extends AppError {
  constructor(message = "Unauthorized") {
    super(message, "UNAUTHORIZED", 401);
  }
}

export class BitrixApiError extends AppError {
  constructor(
    message: string,
    readonly bitrixCode: string,
    statusCode = 502,
  ) {
    super(message, "BITRIX_API_ERROR", statusCode);
  }
}

export function safeErrorMessage(error: unknown): string {
  if (error instanceof AppError) return error.message;
  return "An unexpected internal error occurred";
}
