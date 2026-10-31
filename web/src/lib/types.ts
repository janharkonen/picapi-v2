// Shapes returned by the picapi Go API.

export type Asset = {
	id: string;
	filename: string;
	content_type: string;
	size_bytes: number;
	width: number;
	height: number;
	sha256: string;
	created_at: string;
	url: string;
};

export type AssetPage = { items: Asset[]; next_cursor: string | null };
