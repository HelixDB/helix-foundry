import { z } from "zod";

export const CreateRecordCollectionSchema = z
  .object({
    name: z.string().trim().min(1).max(120),
  })
  .strict();
export const UpdateRecordCollectionSchema = CreateRecordCollectionSchema.extend(
  {
    revision: z.number().int().min(1),
    logicalIds: z.array(z.string().min(1).max(300)).max(200),
  },
);
export const DeleteRecordCollectionSchema = z
  .object({
    revision: z.number().int().min(1),
  })
  .strict();
export type RecordCollection = {
  id: string;
  name: string;
  revision: number;
  updatedAt: string;
  logicalIds: string[];
};
export type RecordCollectionMember = {
  logicalId: string;
  available: boolean;
  name: string | null;
  type: string | null;
};
export type RecordCollectionDetail = RecordCollection & {
  generation: string | null;
  members: RecordCollectionMember[];
};
