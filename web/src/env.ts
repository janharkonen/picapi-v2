import { defineEnvVars } from '@sveltejs/kit/env';

export const variables = defineEnvVars({
	PICAPI_API_URL: {
		description:
			'Base URL of the picapi Go API, reached server-side (in production its Railway private address).',
		schema: (value) => (value || 'http://localhost:8080').replace(/\/+$/, '')
	}
});
