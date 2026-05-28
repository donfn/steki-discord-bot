package registration

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"steki-bot/db"
)

type Repository struct {
	registrations *mongo.Collection
	usedEmails    *mongo.Collection
}

func NewRepository(database *db.DB) *Repository {
	return &Repository{
		registrations: database.Collection("activeRegistrations"),
		usedEmails:    database.Collection("usedEmails"),
	}
}

func (r *Repository) FindByUser(userID string) (*Registration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var reg Registration
	err := r.registrations.FindOne(ctx, bson.M{"user": userID}).Decode(&reg)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &reg, nil
}

func (r *Repository) Upsert(reg *Registration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if reg.ID != nil {
		_, err := r.registrations.UpdateOne(ctx,
			bson.M{"_id": reg.ID},
			bson.M{"$set": reg},
		)
		return err
	}
	result, err := r.registrations.InsertOne(ctx, reg)
	if err != nil {
		return err
	}
	reg.ID = result.InsertedID
	return nil
}

func (r *Repository) Delete(id interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.registrations.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

func (r *Repository) DeleteByUser(userID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.registrations.DeleteOne(ctx, bson.M{"user": userID})
	return err
}

func (r *Repository) EmailExists(hash string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := r.usedEmails.FindOne(ctx, bson.M{"email": hash}).Err()
	if err == mongo.ErrNoDocuments {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *Repository) AddEmail(hash string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.usedEmails.InsertOne(ctx, bson.M{"email": hash})
	return err
}

func (r *Repository) RemoveEmail(hash string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.usedEmails.DeleteOne(ctx, bson.M{"email": hash})
	return err
}
